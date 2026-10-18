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

type syncRecorder struct {
	cons   *Consumer
	video  *core.Receiver
	audio  *core.Receiver
	mu     sync.Mutex
	closed []Segment
}

func newSyncRecorder(t *testing.T, segment time.Duration) *syncRecorder {
	r := &syncRecorder{}
	r.cons = NewConsumer(nil, Options{
		Stream: "cam1", Dir: t.TempDir(), SegmentDuration: segment, MaxDrift: time.Hour,
		OnSegment: func(seg Segment) {
			if !seg.Recording {
				r.mu.Lock()
				r.closed = append(r.closed, seg)
				r.mu.Unlock()
			}
		},
	})

	vmedia := &core.Media{Kind: core.KindVideo, Direction: core.DirectionRecvonly}
	vcodec := &core.Codec{Name: core.CodecH264, ClockRate: 90000, PayloadType: core.PayloadTypeRAW}
	vmedia.Codecs = []*core.Codec{vcodec}
	r.video = core.NewReceiver(vmedia, vcodec)
	require.NoError(t, r.cons.AddTrack(vmedia, vcodec, r.video))

	amedia := &core.Media{Kind: core.KindAudio, Direction: core.DirectionRecvonly}
	acodec := &core.Codec{Name: core.CodecAAC, ClockRate: 16000, Channels: 1, PayloadType: core.PayloadTypeRAW, FmtpLine: "config=1408"}
	amedia.Codecs = []*core.Codec{acodec}
	r.audio = core.NewReceiver(amedia, acodec)
	require.NoError(t, r.cons.AddTrack(amedia, acodec, r.audio))

	r.cons.Start()
	return r
}

const (
	vBase uint32 = 1_000_000 // random RTP timestamp bases, like real cameras
	aBase uint32 = 3_000_000
)

// feed sends seconds of video (25 fps, keyframe every second) and audio,
// the media time t of video is vBase + t*90000, of audio is aBase + t*16000
func (r *syncRecorder) feed(from, to float64) {
	for t := from; t < to-0.001; t += 0.04 {
		frame := int(t*25 + 0.5)
		nalu := []byte{h264.NALUTypePFrame | 0x40, 5}
		if frame%25 == 0 {
			nalu = []byte{h264.NALUTypeIFrame | 0x60, 1}
		}
		r.video.WriteRTP(&rtp.Packet{Header: rtp.Header{Timestamp: vBase + uint32(frame)*3600}, Payload: h264.JoinNALU(nalu)})
		// audio samples for the same time (1024 samples = 64 ms)
		for s := int((t - 0.04) * 16000 / 1024); float64(s)*1024/16000 < t; s++ {
			if s >= 0 && float64(s)*1024/16000 >= t-0.04 {
				r.audio.WriteRTP(&rtp.Packet{Header: rtp.Header{Timestamp: aBase + uint32(s)*1024}, Payload: []byte{0x21}})
			}
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
}

// report sets sender reports: audio media time 0 is audioShift later than video media time 0
func (r *syncRecorder) report(audioShift time.Duration, audioClock time.Duration) {
	now := time.Now()
	ntp := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	r.video.SetSenderReport(&core.SenderReport{NTP: ntp, RTPTime: vBase, Received: now})
	r.audio.SetSenderReport(&core.SenderReport{NTP: ntp.Add(audioShift + audioClock), RTPTime: aBase, Received: now.Add(audioClock)})
}

func (r *syncRecorder) stop() []Segment {
	time.Sleep(20 * time.Millisecond)
	_ = r.cons.Stop()
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closed
}

// audioDts returns decode times of audio and video samples in the segment file
func segmentDts(t *testing.T, seg Segment) (video, audio []uint64, audioTS []uint32) {
	b, err := os.ReadFile(seg.Path)
	require.NoError(t, err)
	entries, err := ReadIndex(IndexPath(seg.Path))
	require.NoError(t, err)
	for _, fr := range parseClip(t, b, b[:entries[0].Offset]) {
		if fr.trackID == 1 {
			video = append(video, fr.dts)
		} else {
			audio = append(audio, fr.dts)
		}
	}
	return
}

// TestSyncSenderReport: audio is aligned by sender reports, not by arrival time
func TestSyncSenderReport(t *testing.T) {
	r := newSyncRecorder(t, time.Hour)

	// the sender says: audio media time 0 is 0.5s after video media time 0
	r.report(500*time.Millisecond, 0)
	r.feed(0, 2)

	segments := r.stop()
	require.Len(t, segments, 1)
	video, audio, _ := segmentDts(t, segments[0])
	require.Equal(t, uint64(0), video[0])
	// first audio sample (media time 0) => 0.5s after the keyframe = 8000 ticks at 16 kHz
	require.Equal(t, uint64(8000), audio[0])
	for i := 1; i < len(audio); i++ {
		require.Equal(t, uint64(1024), audio[i]-audio[i-1])
	}
}

// TestSyncReportLater: the first segment is aligned by arrival time, the next segment by sender reports
func TestSyncReportLater(t *testing.T) {
	r := newSyncRecorder(t, time.Second)

	r.feed(0, 1)
	r.report(500*time.Millisecond, 0) // reports come after the first segment start
	r.feed(1, 3)

	segments := r.stop()
	require.GreaterOrEqual(t, len(segments), 2)

	// Second segment starts at the keyframe with media time 1s: audio sample s (media time s*0.064s)
	// should be at decode time (s*0.064 + 0.5 - 1) * 16000 = s*1024 + 8000 - 16000.
	// Audio before ~1s is in the first segment (aligned by arrival time), so the one-time
	// correction makes 0.5s audio gap on the segment boundary.
	checkSync := func(seg Segment, segStart int) {
		_, audio, _ := segmentDts(t, seg)
		require.NotEmpty(t, audio)
		// the first sample can be s=15 or s=16 (audio and video come from different goroutines)
		v := int64(audio[0]) - 8000 + int64(segStart)*16000 // = s*1024
		require.Zero(t, v%1024, "audio is synchronized by sender reports: %d", audio[0])
		require.InDelta(t, segStart*16000/1024, v/1024, 1)
		for i := 1; i < len(audio); i++ {
			require.Equal(t, uint64(1024), audio[i]-audio[i-1])
		}
	}

	checkSync(segments[1], 1)
	if len(segments) > 2 {
		checkSync(segments[2], 2) // next segments keep the synchronization
	}
}

// TestSyncWrongClock: sender reports from different clocks are ignored (arrival time is used)
func TestSyncWrongClock(t *testing.T) {
	r := newSyncRecorder(t, time.Hour)

	r.report(500*time.Millisecond, time.Hour) // audio report NTP is 1 hour off from its receive time
	r.video.SetSenderReport(&core.SenderReport{NTP: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), RTPTime: vBase, Received: time.Now().Add(-time.Hour)})
	r.feed(0, 2)

	segments := r.stop()
	_, audio, _ := segmentDts(t, segments[0])
	require.Less(t, audio[0], uint64(16000/10), "arrival time alignment (fast feed: few ms)")
}
