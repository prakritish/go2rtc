package record

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"sync"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/aac"
	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/h264"
	"github.com/AlexxIT/go2rtc/pkg/h265"
	"github.com/AlexxIT/go2rtc/pkg/iso"
	"github.com/pion/rtp"
)

type PlayerOptions struct {
	From time.Time
	To   time.Time // zero - follow the recording (live), never ends

	// List returns sorted segments that overlap [from, to), zero to - without limit
	List func(from, to time.Time) []Segment
}

// Player - go2rtc producer that plays recorded segments in real time.
// So the recording can be watched with any go2rtc consumer (WebRTC, RTSP, MSE, MP4, HLS).
// Supported codecs: H264, H265, AAC, Opus and FLAC from PCM (played as PCMA, PCMU or L16).
// Other tracks are skipped.
type Player struct {
	core.Connection

	opts PlayerOptions

	tracks    map[uint32]*core.Codec // supported tracks by track ID (track timescale)
	receivers map[uint32][]*core.Receiver
	mediaIDs  map[*core.Media]uint32

	mu       sync.Mutex
	done     chan struct{}
	stopOnce sync.Once
	playing  sync.WaitGroup // the playback loop is running

	// playback state
	wallStart time.Time          // wall clock for media time 0
	lastTime  map[uint32]float64 // last sent media time for each track
	speed     float64            // pacing speed, only for tests
}

// pollInterval - check for new data when the player reaches the live edge
const pollInterval = 500 * time.Millisecond

func NewPlayer(opts PlayerOptions) (*Player, error) {
	if !opts.To.IsZero() && !opts.To.After(opts.From) {
		return nil, errors.New("record: wrong time range")
	}

	p := &Player{
		Connection: core.Connection{
			ID:         core.NewID(),
			FormatName: "record",
			Protocol:   "file",
		},
		opts:      opts,
		tracks:    map[uint32]*core.Codec{},
		receivers: map[uint32][]*core.Receiver{},
		mediaIDs:  map[*core.Media]uint32{},
		lastTime:  map[uint32]float64{},
		done:      make(chan struct{}),
		speed:     1,
	}

	// tracks from the first segment with data
	seg, clip := p.nextClip(nil)
	if clip == nil {
		return nil, ErrNoRecording
	}

	tracks, ids, err := initCodecs(clip.init)
	if err != nil {
		return nil, err
	}

	for _, id := range ids {
		codec := tracks[id]
		p.tracks[id] = codec

		codecs := []*core.Codec{codec}
		if codec.Name == core.CodecFLAC {
			// FLAC from the recorder is uncompressed 16 bit PCM, convert it back
			codecs = pcmCodecs(codec.ClockRate)
		}

		media := &core.Media{
			Kind:      codec.Kind(),
			Direction: core.DirectionRecvonly,
			Codecs:    codecs,
		}
		p.Medias = append(p.Medias, media)
		p.mediaIDs[media] = id
	}

	if len(p.Medias) == 0 {
		return nil, errors.New("record: no supported codecs in " + seg.ID)
	}

	return p, nil
}

func (p *Player) GetTrack(media *core.Media, codec *core.Codec) (*core.Receiver, error) {
	receiver, err := p.Connection.GetTrack(media, codec)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	if id, ok := p.mediaIDs[media]; ok {
		p.receivers[id] = append(p.receivers[id], receiver)
	}
	p.mu.Unlock()
	return receiver, nil
}

func (p *Player) Start() error {
	p.mu.Lock()
	select {
	case <-p.done:
		p.mu.Unlock()
		return nil // stopped before start
	default:
	}
	p.playing.Add(1)
	p.mu.Unlock()

	err := p.play()
	p.playing.Done()

	// Producer shouldn't exit after the end of the recording, otherwise go2rtc will restart it
	// from the beginning. Wait for the consumers to leave.
	<-p.done

	if errors.Is(err, errStopped) {
		return nil
	}
	return err
}

func (p *Player) Stop() error {
	p.stopOnce.Do(func() {
		p.mu.Lock()
		close(p.done)
		p.mu.Unlock()
	})
	// receivers can't be closed while the playback loop writes to them
	p.playing.Wait()
	return p.Connection.Stop()
}

var errStopped = errors.New("record: player stopped")

// play - main loop over segments
func (p *Player) play() error {
	var prev *Segment
	var segOffset float64 // output time of the current segment start keyframe, seconds

	for {
		seg, clip := p.nextClip(prev)
		if clip == nil {
			if !p.opts.To.IsZero() {
				return nil // end of the requested range
			}
			// live: wait for the next segment
			if err := p.sleep(pollInterval); err != nil {
				return err
			}
			continue
		}

		// first segment: the start keyframe is output time preroll (late audio starts at 0)
		if prev == nil {
			segOffset = clip.Preroll.Seconds()
		}

		duration, err := p.playSegment(seg, clip, segOffset)
		if err != nil {
			return err
		}

		segOffset += duration
		prev = seg
	}
}

// nextClip finds the next segment after prev (or the first segment) and builds its clip
func (p *Player) nextClip(prev *Segment) (*Segment, *Clip) {
	from := p.opts.From
	if prev != nil {
		from = prev.Start.Add(time.Nanosecond)
	}

	for _, seg := range p.opts.List(from, p.opts.To) {
		if prev != nil && !seg.Start.After(prev.Start) {
			continue
		}
		clipFrom := seg.Start
		if prev == nil && p.opts.From.After(clipFrom) {
			clipFrom = p.opts.From
		}
		if clip, err := NewClip([]Segment{seg}, clipFrom, p.clipTo(seg)); err == nil {
			seg := seg
			return &seg, clip
		}
	}
	return nil, nil
}

func (p *Player) clipTo(seg Segment) time.Time {
	if p.opts.To.IsZero() {
		return seg.Start.Add(maxSegmentAge)
	}
	return p.opts.To
}

// maxSegmentAge - upper limit for the segment being recorded
const maxSegmentAge = 24 * time.Hour

// playSegment plays one segment, waits for new data if the segment is being recorded.
// Returns media duration from the start keyframe to the end of the segment (seconds).
func (p *Player) playSegment(seg *Segment, clip *Clip, segOffset float64) (float64, error) {
	clipFrom := seg.Start
	if p.opts.From.After(clipFrom) {
		clipFrom = p.opts.From
	}

	offset := clip.InitSize()

	for {
		// media time relative to the start keyframe = clip time - preroll
		base := segOffset - clip.Preroll.Seconds()

		var err error
		if offset, err = p.playClip(clip, offset, base); err != nil {
			return 0, err
		}

		// clip byte ranges are stable while the segment grows, continue from the offset
		recording := false
		for _, s := range p.opts.List(seg.Start, seg.Start.Add(time.Millisecond)) {
			if s.ID == seg.ID {
				recording = s.Recording
			}
		}

		// the requested range has ended (with a margin for the data written by keyframes)
		if !p.opts.To.IsZero() && time.Now().After(p.opts.To.Add(10*time.Second)) {
			recording = false
		}

		if !recording {
			// segment closed (or removed): check the data written after the last build
			if next, err := NewClip([]Segment{*seg}, clipFrom, p.clipTo(*seg)); err == nil && next.Size() > offset {
				clip = next
				continue
			}
			return (clip.Duration - clip.Preroll).Seconds(), nil
		}

		if err = p.sleep(pollInterval); err != nil {
			return 0, err
		}

		if next, err := NewClip([]Segment{*seg}, clipFrom, p.clipTo(*seg)); err == nil {
			clip = next
		}
	}
}

// playClip sends fragments from the offset till the end of the clip with real time pacing.
// base - output time of the clip media time 0. Returns the new offset.
func (p *Player) playClip(clip *Clip, offset int64, base float64) (int64, error) {
	if offset >= clip.Size() {
		return offset, nil
	}

	rd := clip.NewReader()
	defer rd.Close()

	if _, err := rd.Seek(offset, io.SeekStart); err != nil {
		return offset, err
	}

	br := bufio.NewReaderSize(io.LimitReader(rd, clip.Size()-offset), 256*1024)

	for offset < clip.Size() {
		fr, payload, n, err := readFragment(br)
		if err != nil {
			return offset, err
		}
		offset += n

		codec := p.tracks[fr.trackID]
		if codec == nil {
			continue // unsupported codec
		}

		t := base + float64(fr.dts)/float64(codec.ClockRate)
		if t < 0 {
			continue // late audio before the start of the playback
		}

		// samples that overlap the previous segment (ex. camera sends audio after video when disconnects)
		if last, ok := p.lastTime[fr.trackID]; ok && t <= last {
			continue
		}
		p.lastTime[fr.trackID] = t

		if err = p.wait(t); err != nil {
			return offset, err
		}

		p.mu.Lock()
		receivers := p.receivers[fr.trackID]
		p.mu.Unlock()
		if receivers == nil {
			continue // no consumer for this track
		}

		p.Recv += len(payload)
		timestamp := uint32(int64(math.Round(t*float64(codec.ClockRate))) + int64(fr.cts))

		var samples []byte // decoded FLAC (16 bit big endian)
		if codec.Name == core.CodecFLAC {
			if samples = decodeFLAC(payload); samples == nil {
				continue
			}
		}

		for _, receiver := range receivers {
			data := payload
			if samples != nil {
				data = encodePCM(receiver.Codec.Name, samples)
			}
			receiver.WriteRTP(&rtp.Packet{
				Header:  rtp.Header{Timestamp: timestamp},
				Payload: data,
			})
		}
	}

	return offset, nil
}

// wait until the wall clock time of the media time t
func (p *Player) wait(t float64) error {
	t /= p.speed
	now := time.Now()
	target := p.wallStart.Add(time.Duration(t * float64(time.Second)))

	// first packet or after waiting for the live data: start the clock from now
	if p.wallStart.IsZero() || now.Sub(target) > time.Second {
		p.wallStart = now.Add(-time.Duration(t * float64(time.Second)))
		return p.sleep(0)
	}

	return p.sleep(target.Sub(now))
}

func (p *Player) sleep(d time.Duration) error {
	if d <= 0 {
		select {
		case <-p.done:
			return errStopped
		default:
			return nil
		}
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-p.done:
		return errStopped
	case <-timer.C:
		return nil
	}
}

// readFragment reads one moof+mdat pair, returns the number of bytes read
func readFragment(br *bufio.Reader) (*fragment, []byte, int64, error) {
	header := make([]byte, 8)
	if _, err := io.ReadFull(br, header); err != nil {
		return nil, nil, 0, err
	}
	size := binary.BigEndian.Uint32(header)
	if string(header[4:]) != iso.Moof || size < 8 || size > maxMoofSize {
		return nil, nil, 0, errors.New("record: wrong fragment")
	}

	moof := make([]byte, size)
	copy(moof, header)
	if _, err := io.ReadFull(br, moof[8:]); err != nil {
		return nil, nil, 0, err
	}

	fr := &fragment{}
	if err := parseMoof(moof, fr); err != nil {
		return nil, nil, 0, err
	}

	if _, err := io.ReadFull(br, header); err != nil {
		return nil, nil, 0, err
	}
	mdatSize := binary.BigEndian.Uint32(header)
	if string(header[4:]) != iso.Mdat || mdatSize < 8 {
		return nil, nil, 0, errors.New("record: wrong fragment")
	}

	payload := make([]byte, mdatSize-8)
	if _, err := io.ReadFull(br, payload); err != nil {
		return nil, nil, 0, err
	}

	return fr, payload, int64(size) + int64(mdatSize), nil
}

// initCodecs returns supported codecs by track ID and track IDs in the init order
func initCodecs(init []byte) (map[uint32]*core.Codec, []uint32, error) {
	atoms, err := iso.DecodeAtoms(init)
	if err != nil {
		return nil, nil, err
	}

	codecs := map[uint32]*core.Codec{}
	var ids []uint32
	var trackID, timescale uint32

	for _, atom := range atoms {
		var codec *core.Codec

		switch atom := atom.(type) {
		case *iso.AtomTkhd:
			trackID = atom.TrackID
		case *iso.AtomMdhd:
			timescale = atom.TimeScale
		case *iso.AtomVideo:
			switch atom.Name {
			case "avc1":
				codec = h264.ConfigToCodec(atom.Config)
			case "hev1":
				codec = h265.ConfigToCodec(atom.Config)
			}
		case *iso.AtomAudio:
			if atom.Name == "mp4a" && atom.Config != nil {
				codec = aac.ConfigToCodec(atom.Config)
			}
		case *iso.Atom:
			switch atom.Name {
			case "Opus":
				codec = &core.Codec{Name: core.CodecOpus, ClockRate: 48000, Channels: 2, PayloadType: core.PayloadTypeRAW}
			case "fLaC":
				codec = &core.Codec{Name: core.CodecFLAC, ClockRate: timescale, Channels: 1, PayloadType: core.PayloadTypeRAW}
			}
		}

		if codec != nil && trackID != 0 {
			// decode time is in the track timescale
			if timescale != 0 && codec.ClockRate != timescale {
				codec.ClockRate = timescale
			}
			if _, ok := codecs[trackID]; !ok {
				ids = append(ids, trackID)
			}
			codecs[trackID] = codec
		}
	}

	return codecs, ids, nil
}
