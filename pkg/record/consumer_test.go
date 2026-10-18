package record

import (
	"encoding/binary"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/h264"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

type testRecorder struct {
	cons     *Consumer
	receiver *core.Receiver
	mu       sync.Mutex
	closed   []Segment
}

func newTestRecorder(t *testing.T, dir string) *testRecorder {
	r := &testRecorder{}

	media := &core.Media{Kind: core.KindVideo, Direction: core.DirectionRecvonly}
	codec := &core.Codec{Name: core.CodecH264, ClockRate: 90000, PayloadType: core.PayloadTypeRAW}
	media.Codecs = []*core.Codec{codec}

	r.cons = NewConsumer(nil, Options{
		Stream:          "cam1",
		Dir:             dir,
		SegmentDuration: 2 * time.Second,
		MaxDrift:        time.Hour, // test feeds data faster than real time
		OnSegment: func(seg Segment) {
			if !seg.Recording {
				r.mu.Lock()
				r.closed = append(r.closed, seg)
				r.mu.Unlock()
			}
		},
		OnError: func(err error) {
			t.Error(err)
		},
	})

	r.receiver = core.NewReceiver(media, codec)
	require.NoError(t, r.cons.AddTrack(media, codec, r.receiver))
	r.cons.Start()

	return r
}

// feed sends frames with 25 fps and keyframe every second
func (r *testRecorder) feed(ts uint32, frames int) uint32 {
	for i := 0; i < frames; i++ {
		var nalu []byte
		if i%25 == 0 {
			nalu = []byte{h264.NALUTypeIFrame | 0x60, 1, 2, 3, 4}
		} else {
			nalu = []byte{h264.NALUTypePFrame | 0x40, 5, 6, 7}
		}
		r.receiver.WriteRTP(&rtp.Packet{
			Header:  rtp.Header{Timestamp: ts},
			Payload: h264.JoinNALU(nalu),
		})
		ts += 90000 / 25
		time.Sleep(time.Millisecond) // sender has a small buffer
	}
	return ts
}

func (r *testRecorder) stop() []Segment {
	time.Sleep(50 * time.Millisecond)
	_ = r.cons.Stop()
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closed
}

func countAtoms(t *testing.T, b []byte, name string) (n int) {
	for len(b) >= 8 {
		size := int(binary.BigEndian.Uint32(b))
		require.GreaterOrEqual(t, size, 8)
		require.LessOrEqual(t, size, len(b))
		if string(b[4:8]) == name {
			n++
		}
		b = b[size:]
	}
	require.Empty(t, b)
	return
}

func TestRotation(t *testing.T) {
	dir := t.TempDir()
	r := newTestRecorder(t, dir)

	r.feed(1000, 5*25) // 5 seconds

	segments := r.stop()
	require.Len(t, segments, 3)

	durations := []time.Duration{2 * time.Second, 2 * time.Second, time.Second}

	for i, seg := range segments {
		require.Equal(t, durations[i], seg.Duration, "segment %d", i)
		require.Equal(t, "cam1", seg.Stream)

		// start time = file name + correction from the index
		name, ok := ParseSegmentID(seg.ID)
		require.True(t, ok)
		idx, err := ReadIndex(IndexPath(seg.Path))
		require.NoError(t, err)
		require.True(t, name.Add(IndexStart(idx)).Equal(seg.Start), "segment %d", i)

		b, err := os.ReadFile(seg.Path)
		require.NoError(t, err)
		require.Equal(t, int64(len(b)), seg.Size)

		frames := int(seg.Duration / (time.Second / 25))
		require.Equal(t, 1, countAtoms(t, b, "moov"))
		require.Equal(t, frames, countAtoms(t, b, "moof"))

		entries, err := ReadIndex(IndexPath(seg.Path))
		require.NoError(t, err)

		duration, closed := IndexDuration(entries)
		require.True(t, closed)
		require.Equal(t, seg.Duration, duration)

		// one keyframe per second + optional start correction + end entry
		var keyframes []IndexEntry
		for _, e := range entries {
			if e.Type == EntryKeyframe {
				keyframes = append(keyframes, e)
			}
		}
		require.Len(t, keyframes, int(seg.Duration/time.Second))
		for j, e := range keyframes {
			require.Equal(t, time.Duration(j)*time.Second, e.Time)
			require.Equal(t, "moof", string(b[e.Offset+4:e.Offset+8]))
		}
		require.Equal(t, seg.Size, entries[len(entries)-1].Offset)
	}
}

func TestGap(t *testing.T) {
	dir := t.TempDir()
	r := newTestRecorder(t, dir)

	ts := r.feed(1000, 25)
	r.feed(ts+90000*60, 25) // camera reconnect: timestamp jump

	segments := r.stop()
	require.Len(t, segments, 2)
	require.Equal(t, time.Second, segments[0].Duration)
	require.Equal(t, time.Second, segments[1].Duration)
	require.True(t, segments[1].Start.After(segments[0].Start))
}

func TestWaitKeyframe(t *testing.T) {
	dir := t.TempDir()
	r := newTestRecorder(t, dir)

	// start from P-frames: nothing should be written until keyframe
	r.receiver.WriteRTP(&rtp.Packet{
		Header:  rtp.Header{Timestamp: 0},
		Payload: h264.JoinNALU([]byte{h264.NALUTypePFrame | 0x40, 1}),
	})
	time.Sleep(10 * time.Millisecond)
	require.Nil(t, r.cons.Current())

	r.feed(3600, 25)
	require.NotNil(t, r.cons.Current())

	segments := r.stop()
	require.Len(t, segments, 1)
}

func TestNames(t *testing.T) {
	for _, name := range []string{"cam1", "..", ".", "a/b", "../x", "кам ера"} {
		s := EscapeName(name)
		require.NotContains(t, s, "/")
		require.NotEqual(t, ".", s)
		require.NotEqual(t, "..", s)
		u, err := UnescapeName(s)
		require.NoError(t, err)
		require.Equal(t, name, u)
	}

	start := time.Date(2026, 10, 17, 14, 3, 0, 123e6, time.UTC)
	id := SegmentID(start)
	require.Equal(t, "2026-10-17/2026-10-17T14-03-00.123Z.mp4", id)
	ts, ok := ParseSegmentID(id)
	require.True(t, ok)
	require.True(t, start.Equal(ts))
}

// TestStartTime: the first segment start is corrected to the least delayed frame,
// next segments are chained without overlaps
func TestStartTime(t *testing.T) {
	dir := t.TempDir()
	r := newTestRecorder(t, dir)

	// startup latency: the first 5 frames come late in a burst, then real time 25 fps
	ts := uint32(0)
	for i := 0; i < 5; i++ {
		ts = r.feed(ts, 1)
	}
	time.Sleep(200 * time.Millisecond) // emulate latency at the start
	began := time.Now()
	for i := 5; i < 5*25; i++ {
		ts = r.feed(ts, 1)
		// real time pacing: frame i should arrive at began + (i-5)*40ms
		if d := time.Until(began.Add(time.Duration(i-4) * 40 * time.Millisecond)); d > 0 {
			time.Sleep(d)
		}
	}

	segments := r.stop()
	require.Len(t, segments, 3)

	// first segment: the frames after the burst arrive "earlier" relative to the first keyframe
	name, _ := ParseSegmentID(segments[0].ID)
	require.True(t, segments[0].Start.Before(name), "first segment start should be corrected")

	// chained: each segment starts exactly where the previous one ends
	for i := 1; i < len(segments); i++ {
		require.True(t, segments[i].Start.Equal(segments[i-1].End()), "segment %d: %s != %s", i, segments[i].Start, segments[i-1].End())
	}
}
