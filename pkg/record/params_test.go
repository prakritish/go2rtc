package record

import (
	"os"
	"sync"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/h264"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

// real SPS/PPS: 640x360 and 1280x720 (x264 High profile)
var (
	sps360 = []byte{0x67, 0x64, 0x00, 0x1e, 0xac, 0xd9, 0x40, 0xa0, 0x2f, 0xf9, 0x70, 0x11, 0x00, 0x00, 0x03, 0x00, 0x01, 0x00, 0x00, 0x03, 0x00, 0x32, 0x0f, 0x16, 0x2d, 0x96}
	sps720 = []byte{0x67, 0x64, 0x00, 0x1f, 0xac, 0xd9, 0x40, 0x50, 0x05, 0xbb, 0x01, 0x10, 0x00, 0x00, 0x03, 0x00, 0x10, 0x00, 0x00, 0x03, 0x03, 0x20, 0xf1, 0x83, 0x19, 0x60}
	pps    = []byte{0x68, 0xeb, 0xe3, 0xcb, 0x22, 0xc0}
)

func TestCodecChange(t *testing.T) {
	dir := t.TempDir()
	r := newTestRecorder(t, dir)

	feed := func(sps []byte, ts uint32, frames int) uint32 {
		for i := 0; i < frames; i++ {
			var payload []byte
			if i%25 == 0 {
				payload = h264.JoinNALU(sps, pps, []byte{h264.NALUTypeIFrame | 0x60, 1, 2, 3})
			} else {
				payload = h264.JoinNALU([]byte{h264.NALUTypePFrame | 0x40, 5, 6})
			}
			r.receiver.WriteRTP(&rtp.Packet{Header: rtp.Header{Timestamp: ts}, Payload: payload})
			ts += 90000 / 25
			time.Sleep(time.Millisecond)
		}
		return ts
	}

	ts := feed(sps360, 0, 25) // 1 second
	ts = feed(sps720, ts, 25) // resolution changed without reconnect
	_ = feed(sps720, ts, 25)  // same params: no new segment

	segments := r.stop()
	require.Len(t, segments, 2)
	require.Equal(t, time.Second, segments[0].Duration)
	require.Equal(t, 2*time.Second, segments[1].Duration)
	require.True(t, segments[1].Start.Equal(segments[0].End()), "continuous")

	// each segment has the init with its own resolution
	for i, sps := range [][]byte{sps360, sps720} {
		b, err := os.ReadFile(segments[i].Path)
		require.NoError(t, err)
		entries, err := ReadIndex(IndexPath(segments[i].Path))
		require.NoError(t, err)
		codecs, _, err := initCodecs(b[:entries[0].Offset])
		require.NoError(t, err)
		gotSPS, _ := h264.GetParameterSet(codecs[1].FmtpLine)
		require.Equal(t, sps, gotSPS, "segment %d", i)
	}

	// clip can't contain different inits: it ends at the change
	clip, err := NewClip(segments, segments[0].Start, segments[1].End())
	require.NoError(t, err)
	require.True(t, clip.Truncated)
}

// TestReconnectNewParams: parameter sets only out-of-band (SDP), the camera reconnects
// with the new resolution, go2rtc moves the consumer to the new track
func TestReconnectNewParams(t *testing.T) {
	var mu sync.Mutex
	var closed []Segment
	cons := NewConsumer(nil, Options{
		Stream: "cam1", Dir: t.TempDir(), SegmentDuration: time.Hour, MaxDrift: time.Hour,
		OnSegment: func(seg Segment) {
			if !seg.Recording {
				mu.Lock()
				closed = append(closed, seg)
				mu.Unlock()
			}
		},
	})

	media := &core.Media{Kind: core.KindVideo, Direction: core.DirectionRecvonly}
	codec1 := h264.AVCCToCodec(h264.JoinNALU(sps360, pps))
	media.Codecs = []*core.Codec{codec1}
	recv1 := core.NewReceiver(media, codec1)
	require.NoError(t, cons.AddTrack(media, codec1, recv1))
	cons.Start()

	feed := func(recv *core.Receiver, ts uint32, frames int) uint32 {
		for i := 0; i < frames; i++ {
			nalu := []byte{h264.NALUTypePFrame | 0x40, 5, 6}
			if i%25 == 0 {
				nalu = []byte{h264.NALUTypeIFrame | 0x60, 1, 2, 3} // without SPS and PPS
			}
			recv.WriteRTP(&rtp.Packet{Header: rtp.Header{Timestamp: ts}, Payload: h264.JoinNALU(nalu)})
			ts += 3600
			time.Sleep(time.Millisecond)
		}
		return ts
	}

	feed(recv1, 0, 25)

	// reconnect: the new track with the new codec, timestamps from the new session
	codec2 := h264.AVCCToCodec(h264.JoinNALU(sps720, pps))
	recv2 := core.NewReceiver(media, codec2)
	recv1.Replace(recv2)
	feed(recv2, 1e8, 25)

	time.Sleep(20 * time.Millisecond)
	_ = cons.Stop()

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, closed, 2)
	for i, sps := range [][]byte{sps360, sps720} {
		b, err := os.ReadFile(closed[i].Path)
		require.NoError(t, err)
		entries, err := ReadIndex(IndexPath(closed[i].Path))
		require.NoError(t, err)
		codecs, _, err := initCodecs(b[:entries[0].Offset])
		require.NoError(t, err)
		got, _ := h264.GetParameterSet(codecs[1].FmtpLine)
		require.Equal(t, sps, got, "segment %d init", i)
	}
}
