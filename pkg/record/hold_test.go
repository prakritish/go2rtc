package record

import (
	"os"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/h264"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

// holdRecorder - video 25 fps with keyframe every 0.5 second
type holdRecorder struct {
	cons     *Consumer
	receiver *core.Receiver
	dir      string
	ts       uint32
	mu       sync.Mutex
	segments map[string]Segment
}

func newHoldRecorder(t *testing.T, segment, pre time.Duration) *holdRecorder {
	r := &holdRecorder{dir: t.TempDir(), segments: map[string]Segment{}}
	r.cons = NewConsumer(nil, Options{
		Stream: "cam1", Dir: r.dir, SegmentDuration: segment, MaxDrift: time.Hour,
		Hold: true, PreBuffer: pre,
		OnSegment: func(seg Segment) {
			r.mu.Lock()
			r.segments[seg.ID] = seg
			r.mu.Unlock()
		},
		OnError: func(err error) { t.Error(err) },
	})
	media := &core.Media{Kind: core.KindVideo, Direction: core.DirectionRecvonly}
	codec := &core.Codec{Name: core.CodecH264, ClockRate: 90000, PayloadType: core.PayloadTypeRAW}
	media.Codecs = []*core.Codec{codec}
	r.receiver = core.NewReceiver(media, codec)
	require.NoError(t, r.cons.AddTrack(media, codec, r.receiver))
	r.cons.Start()
	return r
}

// feed frames, returns media time of the last frame
func (r *holdRecorder) feed(frames int) {
	for i := 0; i < frames; i++ {
		n := r.ts / 3600 // frame number
		nalu := []byte{h264.NALUTypePFrame | 0x40, 5, 6, 7}
		if n%12 == 0 {
			nalu = []byte{h264.NALUTypeIFrame | 0x60, byte(n), byte(n >> 8)} // frame number in keyframe
		}
		r.receiver.WriteRTP(&rtp.Packet{Header: rtp.Header{Timestamp: r.ts}, Payload: h264.JoinNALU(nalu)})
		r.ts += 3600
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
}

func (r *holdRecorder) list(t *testing.T) []Segment {
	r.mu.Lock()
	defer r.mu.Unlock()
	var items []Segment
	for _, seg := range r.segments {
		items = append(items, seg)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Start.Before(items[j].Start) })
	return items
}

// firstFrame returns the frame number of the first keyframe in the segment file
func firstFrame(t *testing.T, seg Segment) int {
	b, err := os.ReadFile(seg.Path)
	require.NoError(t, err)
	entries, err := ReadIndex(IndexPath(seg.Path))
	require.NoError(t, err)
	off := entries[0].Offset
	moof := int64(be32(b[off:]))
	payload := b[off+moof+8:]
	return int(payload[5]) | int(payload[6])<<8
}

func be32(b []byte) uint32 {
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

func TestHold(t *testing.T) {
	// segment 4s, pre-buffer 1s, keyframe every 12 frames (0.48s)
	r := newHoldRecorder(t, 4*time.Second, time.Second)

	r.feed(75) // 3 seconds without event: nothing on disk
	require.Empty(t, r.list(t))
	files, _ := os.ReadDir(r.dir)
	require.Empty(t, files, "nothing should be written to disk")

	r.cons.SetHold(false) // event
	segs := r.list(t)
	require.Len(t, segs, 1)
	require.True(t, segs[0].Recording)
	// pre-buffer: keyframes at 48, 60, 72 (frame 74 is the last) => starts from the keyframe 48 (1.92s)
	require.Equal(t, 48, firstFrame(t, segs[0]))

	r.feed(25)           // event: 1 second
	r.cons.SetHold(true) // event ended (post time is handled by the caller)
	r.feed(50)           // 2 seconds after: segment ends on the next keyframe, then memory

	segs = r.list(t)
	require.Len(t, segs, 1)
	require.False(t, segs[0].Recording)
	// frames 48 .. 107 (next keyframe at 108 ends the segment) = 60 frames
	require.Equal(t, 60*40*time.Millisecond, segs[0].Duration)

	// the segment file is valid and continuous
	clip, err := NewClip(segs, segs[0].Start, segs[0].End().Add(time.Second))
	require.NoError(t, err)
	fragments := parseClip(t, readAll(t, clip), clip.init)
	checkTimeline(t, fragments)
	require.Len(t, fragments, 60)
	require.Equal(t, uint64(0), fragments[0].dts)

	_ = r.cons.Stop()
	require.Len(t, r.list(t), 1, "memory segment should not be saved on stop")
}

func TestHoldPrevSegment(t *testing.T) {
	// segment 1s, pre-buffer 1.5s: the pre-buffer starts in the previous memory segment
	r := newHoldRecorder(t, time.Second, 1500*time.Millisecond)

	r.feed(75)
	require.Empty(t, r.list(t))

	r.cons.SetHold(false)
	r.feed(25)
	r.cons.SetHold(true)
	r.feed(25)
	_ = r.cons.Stop()

	segs := r.list(t)
	require.GreaterOrEqual(t, len(segs), 2)

	// pre-buffer starts at least 1.5s before the event (frame 74 => 1.5s = 37 frames => keyframe <= 37)
	require.LessOrEqual(t, firstFrame(t, segs[0]), 37)

	// segments are continuous
	clip, err := NewClip(segs, segs[0].Start, segs[len(segs)-1].End().Add(time.Second))
	require.NoError(t, err)
	require.False(t, clip.Truncated)
	require.Zero(t, clip.Gaps)
	checkTimeline(t, parseClip(t, readAll(t, clip), clip.init))
	for i := 1; i < len(segs); i++ {
		require.True(t, segs[i].Start.Equal(segs[i-1].End()), "segment %d", i)
	}
}

func TestHoldChainedAfterTrim(t *testing.T) {
	// segment 3s, pre-buffer 0.5s: materialize after trimming >1.5s from active segment,
	// then roll over to a second disk segment and verify zero gap between segs[0].End() and segs[1].Start
	r := newHoldRecorder(t, 3*time.Second, 500*time.Millisecond)

	r.feed(50) // 2.0s in memory
	r.cons.SetHold(false)
	r.feed(50) // crosses 3s segment boundary and writes into second segment
	_ = r.cons.Stop()

	segs := r.list(t)
	require.Len(t, segs, 2)
	require.True(t, segs[1].Start.Equal(segs[0].End()), "chained segment start (%s) must equal previous end (%s)", segs[1].Start, segs[0].End())
}

func TestHoldBetweenSegments(t *testing.T) {
	r := newHoldRecorder(t, 4*time.Second, time.Second)

	r.feed(50)           // 2.0s in memory
	r.cons.CloseSegment() // closes active segment into prevMem (c.seg == nil)
	r.cons.SetHold(false) // event arrives before next keyframe
	_ = r.cons.Stop()

	segs := r.list(t)
	require.Len(t, segs, 1, "prevMem should be materialized when SetHold(false) is called with c.seg == nil")
}
