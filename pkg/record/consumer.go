package record

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/h264"
	"github.com/AlexxIT/go2rtc/pkg/h265"
	"github.com/AlexxIT/go2rtc/pkg/mp4"
	"github.com/AlexxIT/go2rtc/pkg/pcm"
	"github.com/pion/rtp"
)

type Options struct {
	Stream string
	Dir    string // stream directory

	SegmentDuration time.Duration // start new segment on first keyframe after this duration
	MaxGap          time.Duration // timestamp gap between frames that breaks the segment
	MaxDrift        time.Duration // allowed difference between media time and wall clock

	OnSegment func(seg Segment) // called on segment open, every keyframe and close
	OnError   func(err error)

	// Hold - start in the hold mode: segments are kept in memory (PreBuffer duration),
	// they are written to disk only after SetHold(false) (events mode)
	Hold      bool
	PreBuffer time.Duration
}

// Consumer - records stream to fMP4 segments with keyframe index
type Consumer struct {
	core.Connection

	opts  Options
	muxer *mp4.Muxer

	mu     sync.Mutex
	codecs []*core.Codec
	master int    // track used for segmentation, video if exists
	clock  uint32 // master track clock rate, 1000 for wall clock (audio only)
	ready  bool
	closed bool

	seg        *segmentWriter
	fragmenter fragmenter
	segStart   time.Time // start time of the current segment (file name)
	chained    bool      // the segment continues the previous one without a break
	minStart   time.Time // the best estimate of the start (min of arrival time - media time)
	nextStart  time.Time // start of the next segment if it continues the current one
	prevEnd    time.Time // end of the previous segment

	hold      bool           // segments are kept in memory
	prevMem   *segmentWriter // previous memory segment (pre-buffer can start in it)
	firstTS   uint32         // master control clock: RTP time for video, wall clock ms for audio only
	lastTS    uint32
	lastDelta uint32
	lastKeyTS uint32
	errUntil  time.Time

	lastPacket atomic.Int64
}

func NewConsumer(medias []*core.Media, opts Options) *Consumer {
	if opts.SegmentDuration <= 0 {
		opts.SegmentDuration = time.Minute
	}
	if opts.MaxGap <= 0 {
		opts.MaxGap = 5 * time.Second
	}
	if opts.MaxDrift <= 0 {
		opts.MaxDrift = 10 * time.Second
	}

	c := &Consumer{
		Connection: core.Connection{
			ID:         core.NewID(),
			FormatName: "record",
			Protocol:   "file",
			Medias:     medias,
		},
		opts:   opts,
		muxer:  &mp4.Muxer{},
		master: -1,
		hold:   opts.Hold,
	}
	c.fragmenter.onFragment = c.writeFragment
	c.fragmenter.maxGap = opts.MaxGap
	return c
}

func (c *Consumer) AddTrack(media *core.Media, _ *core.Codec, track *core.Receiver) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.ready {
		return errors.New("record: can't add track after start")
	}

	trackID := byte(len(c.Senders))

	codec := track.Codec.Clone()
	sender := core.NewSender(media, codec)

	handler := func(packet *rtp.Packet) {
		c.handle(trackID, packet)
	}

	// source timestamps are used (needed for the synchronization by RTCP sender reports)
	timestampsOK := func() bool { return true }

	switch track.Codec.Name {
	case core.CodecH264, core.CodecH265:
		sender.Handler = videoHandler(sender, track.Codec, handler)
	case core.CodecAAC:
		if track.Codec.IsRTP() {
			depay := &aacDepay{handler: handler}
			source := track.Codec
			sender.Handler = func(packet *rtp.Packet) {
				// new source track after reconnect: new RTP session with new timestamps
				if parent := sender.Parent(); parent != nil && parent.Codec != source && parent.Codec != nil {
					source = parent.Codec
					depay.reset()
				}
				depay.Handle(packet)
			}
			timestampsOK = func() bool { return !depay.broken.Load() }
		} else {
			sender.Handler = handler
		}
	case core.CodecOpus, core.CodecMP3:
		sender.Handler = handler
	case core.CodecPCMA, core.CodecPCMU, core.CodecPCM, core.CodecPCML:
		codec.Name = core.CodecFLAC
		if codec.Channels == 2 {
			// hacky way for support two channels audio (same as mp4.Consumer)
			codec.Channels = 1
			codec.ClockRate *= 2
		}
		sender.Handler = pcm.FLACEncoder(track.Codec.Name, codec.ClockRate, handler)
	}

	if sender.Handler == nil {
		return errors.New("record: unsupported codec: " + track.Codec.String())
	}

	if codec.IsVideo() && (c.master < 0 || !c.codecs[c.master].IsVideo()) {
		c.master = int(trackID)
		c.clock = codec.ClockRate
	} else if c.master < 0 {
		c.master = int(trackID)
		c.clock = 1000 // audio only: wall clock in milliseconds
	}

	c.fragmenter.master = c.master

	// own copy: parameter sets can be updated from the stream (see updateParams)
	muxCodec := codec.Clone()
	c.muxer.AddTrack(muxCodec) // used only for init segment
	c.codecs = append(c.codecs, muxCodec)
	c.fragmenter.addTrack(muxCodec, func() *core.SenderReport {
		// the source track (it changes on reconnect)
		if parent := sender.Parent(); parent != nil && timestampsOK() {
			return parent.SenderReport()
		}
		return nil
	})

	sender.HandleRTP(track)
	c.Senders = append(c.Senders, sender)

	return nil
}

// videoHandler depacketizes video. The depacketizer inserts parameter sets (SPS, PPS) from the codec
// into keyframes without them. After the producer reconnect the sender is moved to the new track
// with the new codec (ex. new resolution), so the depacketizer is rebuilt with the new codec.
func videoHandler(sender *core.Sender, codec *core.Codec, handler core.HandlerFunc) core.HandlerFunc {
	newDepay := func(codec *core.Codec) core.HandlerFunc {
		switch {
		case codec.Name == core.CodecH264 && codec.IsRTP():
			return h264.RTPDepay(codec, handler)
		case codec.Name == core.CodecH264:
			return h264.RepairAVCC(codec, handler)
		case codec.IsRTP():
			return h265.RTPDepay(codec, handler)
		default:
			return h265.RepairAVCC(codec, handler)
		}
	}

	depay := newDepay(codec)

	return func(packet *rtp.Packet) {
		if parent := sender.Parent(); parent != nil && parent.Codec != codec && parent.Codec != nil &&
			parent.Codec.Name == codec.Name {
			codec = parent.Codec
			depay = newDepay(codec)
		}
		depay(packet)
	}
}

// Start allows writing. Should be called after all tracks are added,
// because MP4 init segment should contain all tracks.
func (c *Consumer) Start() {
	c.mu.Lock()
	c.ready = true
	c.mu.Unlock()
}

// LastPacket - time of the last received packet (zero if none)
func (c *Consumer) LastPacket() time.Time {
	if ns := c.lastPacket.Load(); ns != 0 {
		return time.Unix(0, ns)
	}
	return time.Time{}
}

// Current - info about the segment being recorded now (nil if none or in memory)
func (c *Consumer) Current() *Segment {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.seg == nil || c.seg.mem {
		return nil
	}
	info := c.segmentInfo()
	return &info
}

// Holding - the segments are kept in memory (no event)
func (c *Consumer) Holding() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hold
}

// SetHold switches the hold mode. hold = false: the pre-buffer (memory) is written to disk
// and the recording continues to disk. hold = true: the current segment is finished
// on the next keyframe and the next segments are kept in memory.
func (c *Consumer) SetHold(hold bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.hold == hold {
		return
	}
	c.hold = hold

	if hold {
		return
	}

	if c.seg == nil || !c.seg.mem || len(c.seg.memGOPs) == 0 {
		if p := c.prevMem; p != nil && len(p.memGOPs) > 0 {
			p.trimTo(p.memDuration - c.opts.PreBuffer)
			if err := p.materialize(c.opts.Dir); err != nil {
				c.fail(err)
			} else if c.opts.OnSegment != nil {
				c.opts.OnSegment(p.info)
			}
		}
		c.prevMem = nil
		return
	}

	// the pre-buffer start: media time in the current segment (can be negative)
	need := c.toDuration(c.lastTS-c.firstTS) - c.opts.PreBuffer
	c.seg.trimTo(need)

	// the pre-buffer starts in the previous memory segment
	if p := c.prevMem; p != nil && len(p.memGOPs) > 0 && c.seg.memGOPs[0].t > need {
		p.trimTo(c.seg.info.Start.Add(need).Sub(p.info.Start))
		if err := p.materialize(c.opts.Dir); err != nil {
			c.fail(err)
		} else if c.opts.OnSegment != nil {
			c.opts.OnSegment(p.info)
		}
	}
	c.prevMem = nil

	if err := c.seg.materialize(c.opts.Dir); err != nil {
		c.fail(err)
		c.seg = nil
		return
	}
	if c.opts.OnSegment != nil {
		c.opts.OnSegment(c.segmentInfo())
	}
}

// CloseSegment finishes current segment, new one will start on next keyframe
func (c *Consumer) CloseSegment() {
	c.mu.Lock()
	c.closeSegment(nil, false)
	c.mu.Unlock()
}

func (c *Consumer) Stop() error {
	c.mu.Lock()
	c.closed = true
	c.closeSegment(nil, false)
	c.mu.Unlock()

	return c.Connection.Stop()
}

func (c *Consumer) handle(trackID byte, packet *rtp.Packet) {
	now := time.Now()
	c.lastPacket.Store(now.UnixNano())

	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.ready || c.closed {
		return
	}

	if int(trackID) != c.master {
		// secondary tracks (audio) are written only inside open segment
		if c.seg != nil {
			c.addSample(trackID, packet, false, now)
		}
		return
	}

	codec := c.codecs[trackID]

	var keyframe bool
	var ts uint32

	switch codec.Name {
	case core.CodecH264:
		keyframe = h264.IsKeyframe(packet.Payload)
		ts = packet.Timestamp
	case core.CodecH265:
		keyframe = h265.IsKeyframe(packet.Payload)
		ts = packet.Timestamp
	default:
		// audio only: wall clock and "keyframe" (index entry) every second
		ts = uint32(now.UnixMilli())
		keyframe = c.seg == nil || c.toDuration(ts-c.lastKeyTS) >= time.Second
	}

	if c.seg != nil {
		// timestamps jump after reconnect or long camera freeze
		if delta := int32(ts - c.lastTS); delta < 0 || c.toDuration(uint32(delta)) > c.opts.MaxGap {
			c.closeSegment(nil, false)
		}
	}

	// camera changed resolution or encoder settings: new segment with the new init
	if keyframe && updateParams(codec, packet.Payload) && c.seg != nil {
		c.closeSegment(&packet.Timestamp, true)
	}

	// event ended: next segment in memory
	if keyframe && c.hold && c.seg != nil && !c.seg.mem {
		c.closeSegment(&packet.Timestamp, true)
	}

	if c.seg != nil && keyframe {
		elapsed := c.toDuration(ts - c.firstTS)
		drift := now.Sub(c.segStart) - elapsed
		if drift < 0 {
			drift = -drift
		}
		// continuous stream: the last frame duration is known exactly
		if drift > c.opts.MaxDrift {
			c.closeSegment(&packet.Timestamp, false) // the next segment starts from the wall clock
		} else if elapsed >= c.opts.SegmentDuration {
			c.closeSegment(&packet.Timestamp, true) // the next segment continues this one
		}
	}

	if c.seg == nil {
		if !keyframe || now.Before(c.errUntil) {
			return
		}
		if !c.openSegment(now, ts) {
			return
		}
	} else {
		c.lastDelta = ts - c.lastTS
	}

	// the least delayed frame gives the best estimate of the segment start
	if est := now.Add(-c.toDuration(ts - c.firstTS)); c.minStart.IsZero() || est.Before(c.minStart) {
		c.minStart = est
	}

	c.lastTS = ts
	if keyframe {
		c.lastKeyTS = ts
	}

	c.addSample(trackID, packet, keyframe, now)
}

func (c *Consumer) openSegment(now time.Time, ts uint32) bool {
	// continuous stream: the segment starts where the previous one ends
	start, chained := now, false
	if !c.nextStart.IsZero() {
		start, chained = c.nextStart, true
		c.nextStart = time.Time{}
	}

	// file name has milliseconds precision, so start time should be the same after restart
	start = start.Truncate(time.Millisecond)

	// protect from overwriting previous segment with the same name
	if !c.segStart.IsZero() && !start.After(c.segStart) {
		start = c.segStart.Add(time.Millisecond)
	}

	init, err := c.muxer.GetInit()
	if err == nil {
		if c.hold {
			c.seg = createMemSegment(c.opts.Stream, start, init, c.opts.PreBuffer)
		} else {
			c.seg, err = createSegment(c.opts.Dir, c.opts.Stream, start, init)
			c.prevMem = nil
		}
	}
	if err != nil {
		c.fail(err)
		return false
	}

	c.seg.info.Codecs = mp4.MimeCodecs(c.codecs)
	c.segStart = start
	c.chained = chained
	c.minStart = time.Time{}
	c.firstTS = ts
	c.lastTS = ts
	// keep lastDelta from previous segment as an estimate of the frame duration

	c.fragmenter.reset(now, ts)

	return true
}

func (c *Consumer) addSample(trackID byte, packet *rtp.Packet, keyframe bool, now time.Time) {
	if err := c.fragmenter.add(trackID, packet, keyframe, now); err != nil {
		c.closeSegment(nil, false)
		c.fail(err)
	}
}

// maxChainError - the chained segment start is corrected if its own estimate is more different
const maxChainError = 500 * time.Millisecond

// closeSegment flushes pending samples and closes the segment.
// nextTS - timestamp of the next master frame, if the stream is continuous.
// chain - the next segment continues this one (its start = this start + duration).
func (c *Consumer) closeSegment(nextTS *uint32, chain bool) {
	if c.seg == nil {
		return
	}

	if err := c.fragmenter.flush(nextTS); err != nil {
		c.fail(err)
	}
	c.fragmenter.carryOver(nextTS)

	seg := c.seg
	c.seg = nil

	// Segment start is the arrival time of the first keyframe, it can be late because of
	// the startup latency (camera, encoder, network). Correct it with the best estimate.
	if !c.minStart.IsZero() {
		est := c.minStart.Add(seg.timeBase).Truncate(time.Millisecond)
		if est.Before(c.prevEnd) {
			est = c.prevEnd // don't overlap the previous segment
		}
		diff := est.Sub(seg.info.Start)
		if diff < 0 {
			diff = -diff
		}
		if (!c.chained && diff > 0) || diff > maxChainError {
			seg.setStart(est)
		}
	}

	duration := c.fragmenter.duration()
	if err := seg.close(duration); err != nil {
		c.fail(err)
	}

	end := seg.info.Start.Add(duration - seg.timeBase) // info.Duration is not set for memory segments
	c.prevEnd = end
	if chain && nextTS != nil {
		c.nextStart = end
	}

	if seg.mem {
		c.prevMem = seg // can be a part of the pre-buffer
		return
	}

	if c.opts.OnSegment != nil {
		c.opts.OnSegment(seg.info)
	}
}

// writeFragment - called by fragmenter for each sample
func (c *Consumer) writeFragment(b []byte, keyframe bool, t time.Duration) error {
	var err error
	if keyframe {
		err = c.seg.writeKeyframe(b, t)
	} else {
		err = c.seg.write(b)
	}
	if err != nil {
		return err
	}

	c.Send += len(b)

	if keyframe && !c.seg.mem && c.opts.OnSegment != nil {
		c.opts.OnSegment(c.segmentInfo())
	}
	return nil
}

func (c *Consumer) segmentInfo() Segment {
	info := c.seg.info
	info.Duration = c.toDuration(c.lastTS-c.firstTS+c.lastDelta) - c.seg.timeBase
	info.Size = c.seg.size
	return info
}

func (c *Consumer) fail(err error) {
	// don't hammer the disk on errors (ex. disk full)
	c.errUntil = time.Now().Add(5 * time.Second)
	if c.opts.OnError != nil {
		c.opts.OnError(err)
	}
}

func (c *Consumer) toDuration(ticks uint32) time.Duration {
	return time.Duration(ticks) * time.Second / time.Duration(c.clock)
}
