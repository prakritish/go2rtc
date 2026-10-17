package record

import (
	"bytes"
	"encoding/binary"
	"io"
	"math/rand"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/h264"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

// recordAV records 5 seconds of video (25 fps, keyframe every second) and AAC audio
// into 2 seconds segments and returns closed segments
func recordAV(t *testing.T) []Segment {
	return recordAVLag(t, 0)
}

// recordAVLag - audio comes later than video for the lag (in video frames)
func recordAVLag(t *testing.T, lag int) []Segment {
	r := newTestRecorder(t, t.TempDir())

	audio := &core.Media{Kind: core.KindAudio, Direction: core.DirectionRecvonly}
	codec := &core.Codec{
		Name: core.CodecAAC, ClockRate: 16000, Channels: 1, PayloadType: core.PayloadTypeRAW,
		FmtpLine: "streamtype=5;profile-level-id=1;mode=AAC-hbr;config=1408",
	}
	audio.Codecs = []*core.Codec{codec}
	receiver := core.NewReceiver(audio, codec)

	// tracks should be added before start
	r.cons.mu.Lock()
	r.cons.ready = false
	r.cons.mu.Unlock()
	require.NoError(t, r.cons.AddTrack(audio, codec, receiver))
	r.cons.Start()

	var vts, ats uint32
	for i := 0; i < 5*25; i++ {
		var nalu []byte
		if i%25 == 0 {
			nalu = []byte{h264.NALUTypeIFrame | 0x60, 1, 2, 3, 4}
		} else {
			nalu = []byte{h264.NALUTypePFrame | 0x40, 5, 6, 7}
		}
		r.receiver.WriteRTP(&rtp.Packet{Header: rtp.Header{Timestamp: vts}, Payload: h264.JoinNALU(nalu)})
		vts += 90000 / 25

		// audio is sent after video, so it never starts before the keyframe
		for float64(ats)/16000 < float64(vts)/90000-float64(lag)/25 {
			receiver.WriteRTP(&rtp.Packet{Header: rtp.Header{Timestamp: ats}, Payload: []byte{0x21, 0x10, 0x05}})
			ats += 1024
		}

		time.Sleep(time.Millisecond)
	}

	segments := r.stop()
	require.Len(t, segments, 3)
	return segments
}

type outFragment struct {
	seq     uint32
	trackID uint32
	dts     uint64
}

// parseClip checks output structure and returns fragments
func parseClip(t *testing.T, b []byte, init []byte) (fragments []outFragment) {
	require.True(t, bytes.HasPrefix(b, init))
	b = b[len(init):]

	for len(b) > 0 {
		size := int(binary.BigEndian.Uint32(b))
		require.Equal(t, "moof", string(b[4:8]))

		fr := &fragment{}
		require.NoError(t, parseMoof(b[:size], fr))
		fragments = append(fragments, outFragment{seq: fr.seq, trackID: fr.trackID, dts: fr.dts})

		b = b[size:]
		size = int(binary.BigEndian.Uint32(b))
		require.Equal(t, "mdat", string(b[4:8]))
		b = b[size:]
	}
	return
}

func readAll(t *testing.T, clip *Clip) []byte {
	rd := clip.NewReader()
	defer rd.Close()
	b, err := io.ReadAll(rd)
	require.NoError(t, err)
	require.Equal(t, clip.Size(), int64(len(b)))
	return b
}

func checkTimeline(t *testing.T, fragments []outFragment) {
	lastDts := map[uint32]uint64{}
	var lastSeq uint32
	for i, fr := range fragments {
		require.Greater(t, fr.seq, lastSeq, "fragment %d", i)
		lastSeq = fr.seq
		if dts, ok := lastDts[fr.trackID]; ok {
			require.GreaterOrEqual(t, fr.dts, dts, "fragment %d", i)
			// no big jumps on segment boundaries
			if fr.trackID == 1 {
				require.LessOrEqual(t, fr.dts-dts, uint64(90000/25*2), "fragment %d", i)
			}
		}
		lastDts[fr.trackID] = fr.dts
	}
}

func TestClipFull(t *testing.T) {
	segments := recordAV(t)

	from := segments[0].Start
	to := segments[2].End().Add(time.Second)

	clip, err := NewClip(segments, from, to)
	require.NoError(t, err)
	require.Len(t, clip.parts, 3)
	require.True(t, clip.Start.Equal(from))
	require.Equal(t, 0, clip.Gaps)
	require.False(t, clip.Truncated)
	require.InDelta(t, 5.0, (clip.Duration - clip.Preroll).Seconds(), 0.05)
	require.Equal(t, "avc1.640029,mp4a.40.2", clip.Codecs)

	b := readAll(t, clip)
	fragments := parseClip(t, b, clip.init)
	checkTimeline(t, fragments)

	// keyframes point to video moof with the same (patched) decode time
	require.Len(t, clip.Keyframes, 5)
	for i, kf := range clip.Keyframes {
		require.Equal(t, time.Duration(i)*time.Second, kf.Time.Round(time.Millisecond), "keyframe %d", i)
		fr := &fragment{offset: kf.Offset}
		size := int64(binary.BigEndian.Uint32(b[kf.Offset:]))
		require.NoError(t, parseMoof(b[kf.Offset:kf.Offset+size], fr))
		require.Equal(t, uint32(1), fr.trackID)
		require.Equal(t, uint64(i*90000), fr.dts)
	}
	require.True(t, clip.Keyframes[0].Real.Equal(segments[0].Start))
	require.True(t, clip.Keyframes[2].Real.Equal(segments[1].Start))

	var video int
	for _, fr := range fragments {
		if fr.trackID == 1 {
			video++
		}
	}
	require.Equal(t, 5*25, video)

	// exact decode time for every frame, without jumps on segment boundaries
	video = 0
	for _, fr := range fragments {
		if fr.trackID == 1 {
			require.Equal(t, uint64(video*90000/25), fr.dts, "frame %d", video)
			video++
		}
	}
	require.Equal(t, uint32(1), fragments[0].trackID)
	require.Equal(t, uint64(0), fragments[0].dts)
}

func TestClipMiddle(t *testing.T) {
	segments := recordAV(t)

	// 2.5s .. 3.7s => starts from keyframe at 2s (segment 2), ends before frame at 3.7s
	from := segments[0].Start.Add(2500 * time.Millisecond)
	to := segments[0].Start.Add(3700 * time.Millisecond)

	// segment start times are wall clock, make them exact for the test
	segments[1].Start = segments[0].Start.Add(2 * time.Second)
	segments[2].Start = segments[0].Start.Add(4 * time.Second)

	clip, err := NewClip(segments, from, to)
	require.NoError(t, err)
	require.Len(t, clip.parts, 1)
	require.True(t, clip.Start.Equal(segments[1].Start), "start %s", clip.Start)
	require.InDelta(t, 1.7, (clip.Duration - clip.Preroll).Seconds(), 0.05) // video only

	fragments := parseClip(t, readAll(t, clip), clip.init)
	checkTimeline(t, fragments)

	var video int
	for _, fr := range fragments {
		if fr.trackID == 1 {
			video++
		}
	}
	// frames 0.00 .. 1.68 (first frame with time >= 1.7 is not included)
	require.Equal(t, 43, video)

	// video starts after preroll (audio samples before the keyframe, here only arrival jitter)
	require.Equal(t, uint32(1), fragments[0].trackID)
	require.Less(t, clip.Preroll, 100*time.Millisecond)
	require.InDelta(t, clip.Preroll.Seconds()*90000, float64(fragments[0].dts), 1)
}

func TestClipAcrossSegments(t *testing.T) {
	segments := recordAV(t)
	segments[1].Start = segments[0].Start.Add(2 * time.Second)
	segments[2].Start = segments[0].Start.Add(4 * time.Second)

	from := segments[0].Start.Add(1500 * time.Millisecond)
	to := segments[0].Start.Add(4500 * time.Millisecond)

	clip, err := NewClip(segments, from, to)
	require.NoError(t, err)
	require.Len(t, clip.parts, 3)
	require.True(t, clip.Start.Equal(segments[0].Start.Add(time.Second)))
	require.InDelta(t, 3.5, (clip.Duration - clip.Preroll).Seconds(), 0.04) // up to 1 frame, video only

	checkTimeline(t, parseClip(t, readAll(t, clip), clip.init))
}

func TestClipRange(t *testing.T) {
	segments := recordAV(t)

	clip, err := NewClip(segments, segments[0].Start, segments[2].End())
	require.NoError(t, err)

	full := readAll(t, clip)

	rd := clip.NewReader()
	defer rd.Close()

	rnd := rand.New(rand.NewSource(1))
	for i := 0; i < 200; i++ {
		offset := rnd.Int63n(clip.Size())
		size := 1 + rnd.Intn(4096)

		_, err = rd.Seek(offset, io.SeekStart)
		require.NoError(t, err)

		b := make([]byte, size)
		n, err := io.ReadFull(rd, b)
		if err != nil {
			require.ErrorIs(t, err, io.ErrUnexpectedEOF)
		}
		require.Equal(t, full[offset:offset+int64(n)], b[:n], "offset %d size %d", offset, size)
	}
}

func TestClipGapAndEmpty(t *testing.T) {
	segments := recordAV(t)

	// camera was offline for 10 seconds before the last segment
	segments[2].Start = segments[1].End().Add(10 * time.Second)

	clip, err := NewClip(segments, segments[0].Start, segments[2].End())
	require.NoError(t, err)
	require.Equal(t, 1, clip.Gaps)
	require.InDelta(t, 5.0, (clip.Duration - clip.Preroll).Seconds(), 0.05) // gaps are skipped
	checkTimeline(t, parseClip(t, readAll(t, clip), clip.init))

	// inside the gap
	_, err = NewClip(segments, segments[1].End().Add(time.Second), segments[1].End().Add(5*time.Second))
	require.ErrorIs(t, err, ErrNoRecording)

	// before all segments
	_, err = NewClip(segments, segments[0].Start.Add(-time.Hour), segments[0].Start.Add(-time.Minute))
	require.ErrorIs(t, err, ErrNoRecording)
}

func TestAudioJump(t *testing.T) {
	r := newTestRecorder(t, t.TempDir())

	audio := &core.Media{Kind: core.KindAudio, Direction: core.DirectionRecvonly}
	codec := &core.Codec{
		Name: core.CodecAAC, ClockRate: 16000, Channels: 1, PayloadType: core.PayloadTypeRAW,
		FmtpLine: "config=1408",
	}
	audio.Codecs = []*core.Codec{codec}
	receiver := core.NewReceiver(audio, codec)

	r.cons.mu.Lock()
	r.cons.ready = false
	r.cons.mu.Unlock()
	require.NoError(t, r.cons.AddTrack(audio, codec, receiver))
	r.cons.Start()

	sendAudio := func(ts uint32, n int) {
		for i := 0; i < n; i++ {
			receiver.WriteRTP(&rtp.Packet{Header: rtp.Header{Timestamp: ts}, Payload: []byte{0x21}})
			ts += 1024
			time.Sleep(time.Millisecond)
		}
	}

	r.feed(0, 1)      // keyframe opens the segment
	sendAudio(0, 10)  // normal audio
	sendAudio(5e8, 5) // reconnect: new audio session before video jump

	segments := r.stop()
	require.Len(t, segments, 1)

	b, err := os.ReadFile(segments[0].Path)
	require.NoError(t, err)

	entries, err := ReadIndex(IndexPath(segments[0].Path))
	require.NoError(t, err)

	var audioDts []uint64
	for _, fr := range parseClip(t, b, b[:entries[0].Offset]) {
		if fr.trackID == 2 {
			audioDts = append(audioDts, fr.dts)
		}
	}
	require.Len(t, audioDts, 10, "audio from the new session should be skipped")
	require.Equal(t, uint64(9*1024), audioDts[9]-audioDts[0])
	require.Less(t, audioDts[0], uint64(16000/10), "audio is aligned by arrival time")
}

func TestAudioLag(t *testing.T) {
	// audio comes 10 frames (0.4s) later than video
	segments := recordAVLag(t, 10)

	// segment start times are wall clock, make them exact for the test (data is fed faster than real time)
	segments[1].Start = segments[0].Start.Add(2 * time.Second)
	segments[2].Start = segments[0].Start.Add(4 * time.Second)

	clip, err := NewClip(segments, segments[0].Start, segments[2].End().Add(time.Second))
	require.NoError(t, err)

	fragments := parseClip(t, readAll(t, clip), clip.init)
	checkTimeline(t, fragments)

	// audio must be continuous: 1024 samples step without overlaps and holes on segment boundaries
	var audioDts []uint64
	for _, fr := range fragments {
		if fr.trackID == 2 {
			audioDts = append(audioDts, fr.dts)
		}
	}
	require.Len(t, audioDts, 72) // 4.6s of audio (5s video - 0.4s lag)
	for i := 1; i < len(audioDts); i++ {
		require.Equal(t, uint64(1024), audioDts[i]-audioDts[i-1], "audio sample %d", i)
	}

	// second segment: video starts with lead = first segment duration - its audio end,
	// so late audio (that belongs to the first segment time) stays in place
	b, err := os.ReadFile(segments[0].Path)
	require.NoError(t, err)
	entries0, err := ReadIndex(IndexPath(segments[0].Path))
	require.NoError(t, err)
	var audioEnd uint64
	for _, fr := range parseClip(t, b, b[:entries0[0].Offset]) {
		if fr.trackID == 2 {
			audioEnd = fr.dts + 1024
		}
	}
	lead := 2.0 - float64(audioEnd)/16000
	require.Greater(t, lead, 0.2)

	f, err := os.Open(segments[1].Path)
	require.NoError(t, err)
	defer f.Close()
	entries, err := ReadIndex(IndexPath(segments[1].Path))
	require.NoError(t, err)
	require.Equal(t, time.Duration(0), entries[0].Time)
	dts, ok := fragmentDts(f, entries[0].Offset, segments[1].Size, 90000)
	require.True(t, ok)
	require.InDelta(t, lead, dts, 1.0/90000)
	require.InDelta(t, 2.0, segments[1].Duration.Seconds(), 0.001)
}

func TestClipPreroll(t *testing.T) {
	// audio comes 10 frames (0.4s) later than video
	segments := recordAVLag(t, 10)
	segments[1].Start = segments[0].Start.Add(2 * time.Second)
	segments[2].Start = segments[0].Start.Add(4 * time.Second)

	// start from the middle of the second segment (keyframe at 3s)
	from := segments[0].Start.Add(3500 * time.Millisecond)
	clip, err := NewClip(segments, from, from.Add(time.Second))
	require.NoError(t, err)
	require.True(t, clip.Start.Equal(segments[0].Start.Add(3*time.Second)))

	// audio that belongs to 2.6-3.0s is written after the keyframe at 3.0s
	require.InDelta(t, 0.4, clip.Preroll.Seconds(), 0.1)

	fragments := parseClip(t, readAll(t, clip), clip.init)
	checkTimeline(t, fragments)

	// video starts at preroll, audio is continuous from 0 without clamped samples
	require.InDelta(t, clip.Preroll.Seconds()*90000, float64(fragments[0].dts), 1)
	var audioDts []uint64
	for _, fr := range fragments {
		if fr.trackID == 2 {
			audioDts = append(audioDts, fr.dts)
		}
	}
	require.Less(t, audioDts[0], uint64(16000*0.05))
	for i := 1; i < len(audioDts); i++ {
		require.Equal(t, uint64(1024), audioDts[i]-audioDts[i-1], "audio sample %d", i)
	}
}

func TestClipConcurrentReaders(t *testing.T) {
	segments := recordAV(t)

	clip, err := NewClip(segments, segments[0].Start, segments[2].End().Add(time.Second))
	require.NoError(t, err)

	full := readAll(t, clip)

	var wg sync.WaitGroup
	errs := make(chan string, 8)
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			rd := clip.NewReader()
			defer rd.Close()
			rnd := rand.New(rand.NewSource(seed))
			for i := 0; i < 100; i++ {
				offset := rnd.Int63n(clip.Size())
				b := make([]byte, 1+rnd.Intn(8192))
				_, _ = rd.Seek(offset, io.SeekStart)
				n, _ := io.ReadFull(rd, b)
				if !bytes.Equal(full[offset:offset+int64(n)], b[:n]) {
					errs <- "mismatch"
					return
				}
			}
		}(int64(g))
	}
	wg.Wait()
	close(errs)
	require.Empty(t, errs)
}

// TestClipDisconnect - camera sends audio after video when disconnects, then reconnects with new timestamps
func TestClipDisconnect(t *testing.T) {
	r := newTestRecorder(t, t.TempDir())

	audio := &core.Media{Kind: core.KindAudio, Direction: core.DirectionRecvonly}
	codec := &core.Codec{Name: core.CodecAAC, ClockRate: 16000, Channels: 1, PayloadType: core.PayloadTypeRAW, FmtpLine: "config=1408"}
	audio.Codecs = []*core.Codec{codec}
	receiver := core.NewReceiver(audio, codec)
	r.cons.mu.Lock()
	r.cons.ready = false
	r.cons.mu.Unlock()
	require.NoError(t, r.cons.AddTrack(audio, codec, receiver))
	r.cons.Start()

	session := func(vts, ats uint32, frames, extraAudio int) {
		a0 := ats
		for i := 0; i < frames; i++ {
			nalu := []byte{h264.NALUTypePFrame | 0x40, 5, 6, 7}
			if i%25 == 0 {
				nalu = []byte{h264.NALUTypeIFrame | 0x60, 1, 2, 3, 4}
			}
			r.receiver.WriteRTP(&rtp.Packet{Header: rtp.Header{Timestamp: vts}, Payload: h264.JoinNALU(nalu)})
			vts += 90000 / 25
			for float64(ats-a0)/16000 < float64(i+1)/25 {
				receiver.WriteRTP(&rtp.Packet{Header: rtp.Header{Timestamp: ats}, Payload: []byte{0x21}})
				ats += 1024
			}
			time.Sleep(time.Millisecond)
		}
		// audio continues after video
		for i := 0; i < extraAudio; i++ {
			receiver.WriteRTP(&rtp.Packet{Header: rtp.Header{Timestamp: ats}, Payload: []byte{0x21}})
			ats += 1024
			time.Sleep(time.Millisecond)
		}
	}

	session(0, 0, 50, 16)    // 2s video + 1s extra audio
	session(5e8, 7e7, 50, 0) // reconnect: new timestamps
	segments := r.stop()
	require.Len(t, segments, 2)
	segments[1].Start = segments[0].Start.Add(3 * time.Second) // exact for the test
	clip, err := NewClip(segments, segments[0].Start, segments[1].End().Add(time.Second))
	require.NoError(t, err)

	fragments := parseClip(t, readAll(t, clip), clip.init)
	checkTimeline(t, fragments) // monotonic

	var video, audioN int
	for _, fr := range fragments {
		if fr.trackID == 1 {
			video++
		} else {
			audioN++
		}
	}
	require.Equal(t, 100, video)
	// first part audio is cut where the second part audio starts (~2s), extra audio is removed
	require.InDelta(t, 2*2*16000/1024, audioN, 3)

	// the last video frame of the first session is written before the extra audio
	b, err := os.ReadFile(segments[0].Path)
	require.NoError(t, err)
	entries, err := ReadIndex(IndexPath(segments[0].Path))
	require.NoError(t, err)
	frs := parseClip(t, b, b[:entries[0].Offset])
	require.Equal(t, uint32(2), frs[len(frs)-1].trackID, "file should end with audio")
}
