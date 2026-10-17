package record

import (
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/h264"
	"github.com/AlexxIT/go2rtc/pkg/h265"
	"github.com/AlexxIT/go2rtc/pkg/iso"
	"github.com/pion/rtp"
)

// fragmenter makes one fMP4 fragment per sample, like mp4.Muxer, but decode time of each
// sample is equal to its timestamp relative to the segment start (without mp4.Muxer one
// frame shift). Non-keyframe samples are held until the next sample of the same track
// to know their exact duration.
type fragmenter struct {
	tracks []*fragmentTrack
	master int
	seq    uint32
	maxGap time.Duration

	segWall  time.Time     // wall clock of the segment start
	carry    time.Duration // master duration of the previous segment
	lead     time.Duration // how much secondary tracks are behind the master at the segment boundary
	hasCarry bool          // previous segment was closed without a break in the stream

	onFragment func(b []byte, keyframe bool, time time.Duration) error
}

type fragmentTrack struct {
	codec   *core.Codec
	started bool
	first   uint32 // timestamp that corresponds to the offset decode time
	offset  int64  // decode time of the first sample
	lastDts uint64
	lastDur uint32
	end     uint64 // decode time of the end of the last written sample
	pending *pendingSample
	broken  bool // timestamps jump on secondary track, skip it till the next segment
}

type pendingSample struct {
	payload  []byte
	ts       uint32
	cts      uint32
	keyframe bool
}

// masterDelayTolerance - see masterDelay
const masterDelayTolerance = 10 * time.Millisecond

// masterDelay - secondary samples after this time from the pending master sample mean
// that the next master sample is late (usually audio is behind video, so it happens only when the video stops)
func (t *fragmentTrack) masterDelay() time.Duration {
	duration := t.lastDur
	if duration == 0 {
		duration = defaultDuration(t.codec)
	}
	return toDuration(int64(duration), t.codec.ClockRate) + masterDelayTolerance
}

// time of the sample with the timestamp in the segment
func (t *fragmentTrack) time(ts uint32) time.Duration {
	return toDuration(int64(int32(ts-t.first))+t.offset, t.codec.ClockRate)
}

func (f *fragmenter) addTrack(codec *core.Codec) {
	f.tracks = append(f.tracks, &fragmentTrack{codec: codec})
}

// maxLead - max secondary track (audio) delay that is compensated on the segment boundary
const maxLead = 2 * time.Second

// carryOver should be called after flush on the segment close.
// nextTS - timestamp of the next master sample if the stream is continuous.
func (f *fragmenter) carryOver(nextTS *uint32) {
	master := f.tracks[f.master]
	if nextTS == nil || !master.started {
		f.hasCarry = false
		return
	}

	ticks := int64(*nextTS-master.first) + master.offset
	f.carry = toDuration(ticks, master.codec.ClockRate)
	f.hasCarry = true

	// Audio usually comes later than video (encoder and network buffers). So samples
	// before the master keyframe that starts the new segment can come after it.
	// The new segment starts master track with this lead to keep them in place.
	f.lead = 0
	for i, track := range f.tracks {
		if i == f.master || !track.started || track.broken || track.end == 0 {
			continue
		}
		if lead := f.carry - toDuration(int64(track.end), track.codec.ClockRate); lead > f.lead && lead <= maxLead {
			f.lead = lead
		}
	}
}

// reset - start new segment at the wall clock time.
// Secondary tracks keep their sync with the master track if the stream is continuous.
// Otherwise, their first sample is aligned by the wall clock (arrival time).
func (f *fragmenter) reset(now time.Time) {
	for i, track := range f.tracks {
		if f.hasCarry && i != f.master && track.started && !track.broken {
			// new decode time = old decode time - carry + lead (carry includes old offsets)
			track.first += uint32(toTicks(f.carry, track.codec.ClockRate))
			track.offset += toTicks(f.lead, track.codec.ClockRate)
		} else {
			track.started = false
			track.offset = 0
		}
		if i == f.master && f.hasCarry {
			track.offset = toTicks(f.lead, track.codec.ClockRate)
		}
		track.pending = nil
		track.lastDts = 0
		track.end = 0
		track.broken = false
	}

	f.seq = 0
	f.segWall = now
	f.hasCarry = false
}

func toDuration(ticks int64, clockRate uint32) time.Duration {
	return time.Duration(ticks) * time.Second / time.Duration(clockRate)
}

func toTicks(d time.Duration, clockRate uint32) int64 {
	return int64(d * time.Duration(clockRate) / time.Second)
}

// add sample to the track, previous sample of this track is written with exact duration
func (f *fragmenter) add(trackID byte, packet *rtp.Packet, keyframe bool, now time.Time) error {
	track := f.tracks[trackID]

	if !track.started {
		track.started = true
		track.first = packet.Timestamp
		if int(trackID) != f.master {
			// RTP timestamps of different tracks are not related, use arrival time
			if d := now.Sub(f.segWall); d > 0 {
				track.offset = int64(d * time.Duration(track.codec.ClockRate) / time.Second)
			}
		}
	}

	if track.broken {
		return nil
	}

	if track.pending != nil {
		// After reconnect, secondary track (audio) can get new timestamps before
		// master track (video) breaks the segment. These samples are from another session.
		if int(trackID) != f.master {
			delta := int32(packet.Timestamp - track.pending.ts)
			if delta < 0 || time.Duration(delta)*time.Second/time.Duration(track.codec.ClockRate) > f.maxGap {
				track.broken = true
				return f.write(trackID, 0)
			}
		}

		if err := f.write(trackID, packet.Timestamp-track.pending.ts); err != nil {
			return err
		}
	}

	// Master sample waits for the next one to know its duration. If the master track stops
	// (camera disconnects) and secondary samples go ahead, write it, so the file order
	// follows the time (it's important for cutting the overlapping audio in the clip).
	if int(trackID) != f.master {
		if m := f.tracks[f.master]; m.pending != nil && track.time(packet.Timestamp)-m.time(m.pending.ts) > m.masterDelay() {
			if err := f.write(byte(f.master), 0); err != nil {
				return err
			}
		}
	}

	sample := &pendingSample{
		// payload buffer may be reused by depacketizer
		payload: append([]byte(nil), packet.Payload...),
		ts:      packet.Timestamp,
		// ExtensionProfile - wrong place for CTS (same as mp4.Muxer)
		cts:      uint32(packet.ExtensionProfile),
		keyframe: keyframe,
	}

	if keyframe {
		// Keyframe is written immediately with estimated duration (decode time is exact),
		// after all pending samples. So all samples before the keyframe are before it in the file:
		// - the first fragment after init is always the keyframe
		// - playback from the keyframe offset doesn't lose any samples
		if err := f.flush(&packet.Timestamp); err != nil {
			return err
		}
		track.pending = sample
		return f.write(trackID, 0)
	}

	track.pending = sample
	return nil
}

// flush writes pending samples, nextTS - timestamp of the next master sample (if known)
func (f *fragmenter) flush(nextTS *uint32) (err error) {
	for i, track := range f.tracks {
		if track.pending == nil {
			continue
		}
		var duration uint32
		if i == f.master && nextTS != nil {
			duration = *nextTS - track.pending.ts
		}
		if err1 := f.write(byte(i), duration); err == nil {
			err = err1
		}
	}
	return
}

// duration of the master track in the current segment (from the first keyframe)
func (f *fragmenter) duration() time.Duration {
	if f.master < 0 || f.master >= len(f.tracks) {
		return 0
	}
	track := f.tracks[f.master]
	if track.end == 0 {
		return 0
	}
	return toDuration(int64(track.end)-track.offset, track.codec.ClockRate)
}

func (f *fragmenter) write(trackID byte, duration uint32) error {
	track := f.tracks[trackID]
	sample := track.pending
	track.pending = nil

	// wrong duration: timestamps jump or B-frames (timestamp is PTS)
	if int32(duration) <= 0 || duration > track.codec.ClockRate {
		duration = track.lastDur
		if duration == 0 {
			duration = defaultDuration(track.codec)
		}
	} else {
		track.lastDur = duration
	}

	var dts uint64
	if v := int64(int32(sample.ts-track.first)) + track.offset; v > 0 {
		dts = uint64(v)
	}
	if track.end > 0 && dts <= track.lastDts {
		dts = track.lastDts + 1 // decode time should always increase (track.end > 0 - not first sample)
	}
	track.lastDts = dts
	track.end = dts + uint64(duration)

	var flags uint32
	switch track.codec.Name {
	case core.CodecH264:
		flags = sampleFlags(h264.IsKeyframe(sample.payload))
	case core.CodecH265:
		flags = sampleFlags(h265.IsKeyframe(sample.payload))
	default:
		flags = iso.SampleAudio
	}

	f.seq++

	mv := iso.NewMovie(1024 + len(sample.payload))
	mv.WriteMovieFragment(
		f.seq, uint32(trackID)+1, duration, uint32(len(sample.payload)), flags, dts, sample.cts,
	)
	mv.WriteData(sample.payload)

	// index time is relative to the segment start (master keyframe), without the lead
	t := toDuration(int64(dts)-track.offset, track.codec.ClockRate)
	return f.onFragment(mv.Bytes(), sample.keyframe, t)
}

func sampleFlags(keyframe bool) uint32 {
	if keyframe {
		return iso.SampleVideoIFrame
	}
	return iso.SampleVideoNonIFrame
}

func defaultDuration(codec *core.Codec) uint32 {
	switch {
	case codec.Name == core.CodecAAC:
		return 1024
	case codec.IsVideo():
		return codec.ClockRate / 25
	default:
		return codec.ClockRate / 50
	}
}
