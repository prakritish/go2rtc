package record

import (
	"encoding/binary"
	"fmt"
	"io"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/h264"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

// TestClipLive builds clips while the segment is being recorded
func TestClipLive(t *testing.T) {
	var mu sync.Mutex
	segments := map[string]Segment{}

	cons := NewConsumer(nil, Options{
		Stream: "cam1", Dir: t.TempDir(), SegmentDuration: 2 * time.Second, MaxDrift: time.Hour,
		OnSegment: func(seg Segment) {
			mu.Lock()
			segments[seg.ID] = seg
			mu.Unlock()
		},
	})

	video := &core.Media{Kind: core.KindVideo, Direction: core.DirectionRecvonly}
	vcodec := &core.Codec{Name: core.CodecH264, ClockRate: 90000, PayloadType: core.PayloadTypeRAW}
	video.Codecs = []*core.Codec{vcodec}
	vrecv := core.NewReceiver(video, vcodec)
	require.NoError(t, cons.AddTrack(video, vcodec, vrecv))

	audio := &core.Media{Kind: core.KindAudio, Direction: core.DirectionRecvonly}
	acodec := &core.Codec{Name: core.CodecAAC, ClockRate: 16000, Channels: 1, PayloadType: core.PayloadTypeRAW, FmtpLine: "config=1408"}
	audio.Codecs = []*core.Codec{acodec}
	arecv := core.NewReceiver(audio, acodec)
	require.NoError(t, cons.AddTrack(audio, acodec, arecv))
	cons.Start()

	done := make(chan struct{})
	go func() {
		defer close(done)
		var vts, ats uint32
		for i := 0; i < 6*25; i++ {
			var nalu []byte
			if i%25 == 0 {
				nalu = append([]byte{h264.NALUTypeIFrame | 0x60}, make([]byte, 20000)...) // big keyframe
			} else {
				nalu = append([]byte{h264.NALUTypePFrame | 0x40}, make([]byte, 3000)...)
			}
			vrecv.WriteRTP(&rtp.Packet{Header: rtp.Header{Timestamp: vts}, Payload: h264.JoinNALU(nalu)})
			vts += 90000 / 25
			for float64(ats)/16000 < float64(vts)/90000 {
				arecv.WriteRTP(&rtp.Packet{Header: rtp.Header{Timestamp: ats}, Payload: make([]byte, 200)})
				ats += 1024
			}
			time.Sleep(4 * time.Millisecond)
		}
	}()

	var checked, failed int
	for running := true; running; {
		select {
		case <-done:
			running = false
		default:
		}

		mu.Lock()
		list := make([]Segment, 0, len(segments))
		for _, seg := range segments {
			list = append(list, seg)
		}
		mu.Unlock()
		if len(list) == 0 {
			continue
		}
		sort.Slice(list, func(i, j int) bool { return list[i].Start.Before(list[j].Start) })

		// different start points inside the recorded range
		for _, back := range []time.Duration{0, 500 * time.Millisecond, 1500 * time.Millisecond} {
			from := list[len(list)-1].Start.Add(-back)
			clip, err := NewClip(list, from, from.Add(time.Hour))
			if err != nil {
				continue
			}
			rd := clip.NewReader()
			b, err := io.ReadAll(rd)
			_ = rd.Close()
			require.NoError(t, err)
			require.Equal(t, clip.Size(), int64(len(b)))

			checked++
			if msg := checkBoxes(b, clip.InitSize()); msg != "" {
				failed++
				if failed < 5 {
					t.Logf("BAD: %s (size=%d parts=%d)", msg, len(b), len(clip.parts))
				}
			}
		}
	}

	_ = cons.Stop()
	t.Logf("checked=%d failed=%d", checked, failed)
	require.Greater(t, checked, 10)
	require.Zero(t, failed)
}

func checkBoxes(b []byte, i int64) string {
	for i < int64(len(b)) {
		if i+8 > int64(len(b)) {
			return fmt.Sprintf("tail %d bytes at %d", int64(len(b))-i, i)
		}
		size := int64(binary.BigEndian.Uint32(b[i:]))
		name := string(b[i+4 : i+8])
		if size < 8 || (name != "moof" && name != "mdat") || i+size > int64(len(b)) {
			return fmt.Sprintf("box %q size=%d at %d", name, size, i)
		}
		i += size
	}
	return ""
}
