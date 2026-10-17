package record

import (
	"bytes"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/h264"
	"github.com/AlexxIT/go2rtc/pkg/pcm"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

type playerPacket struct {
	kind string
	ts   uint32
	key  bool
}

// listFunc - like catalog.list for the test segments
func listFunc(mu *sync.Mutex, segments func() []Segment) func(from, to time.Time) []Segment {
	return func(from, to time.Time) []Segment {
		mu.Lock()
		defer mu.Unlock()
		var result []Segment
		for _, seg := range segments() {
			if !to.IsZero() && !seg.Start.Before(to) {
				continue
			}
			if !seg.End().After(from) && !seg.Recording {
				continue
			}
			result = append(result, seg)
		}
		sort.Slice(result, func(i, j int) bool { return result[i].Start.Before(result[j].Start) })
		return result
	}
}

// consume attaches consumers to all player tracks
func consume(t *testing.T, p *Player) (func() []playerPacket, func()) {
	var mu sync.Mutex
	var packets []playerPacket

	var senders []*core.Sender
	for _, media := range p.GetMedias() {
		codec := media.Codecs[0]
		receiver, err := p.GetTrack(media, codec)
		require.NoError(t, err)

		kind := media.Kind
		isH264 := codec.Name == core.CodecH264
		sender := core.NewSender(media, codec)
		sender.Handler = func(pkt *core.Packet) {
			mu.Lock()
			packets = append(packets, playerPacket{kind: kind, ts: pkt.Timestamp, key: isH264 && isKeyframe(pkt.Payload)})
			mu.Unlock()
		}
		sender.HandleRTP(receiver)
		senders = append(senders, sender)
	}

	get := func() []playerPacket {
		mu.Lock()
		defer mu.Unlock()
		return append([]playerPacket(nil), packets...)
	}
	stop := func() {
		for _, s := range senders {
			s.Close()
		}
	}
	return get, stop
}

func isKeyframe(b []byte) bool {
	return len(b) > 4 && b[4]&0x1F == 5
}

func checkVideo(t *testing.T, packets []playerPacket) (frames int) {
	var last uint32
	for i, pkt := range packets {
		if pkt.kind != core.KindVideo {
			continue
		}
		if frames == 0 {
			require.True(t, pkt.key, "first video packet should be keyframe")
		} else {
			require.Equal(t, uint32(3600), pkt.ts-last, "packet %d", i)
		}
		last = pkt.ts
		frames++
	}
	return
}

func TestPlayerRange(t *testing.T) {
	segments := recordAV(t)
	segments[1].Start = segments[0].Start.Add(2 * time.Second)
	segments[2].Start = segments[0].Start.Add(4 * time.Second)

	var mu sync.Mutex
	p, err := NewPlayer(PlayerOptions{
		From: segments[0].Start.Add(1500 * time.Millisecond), // keyframe at 1s
		To:   segments[0].Start.Add(10 * time.Second),
		List: listFunc(&mu, func() []Segment { return segments }),
	})
	require.NoError(t, err)
	p.speed = 20

	require.Len(t, p.GetMedias(), 2)
	require.Equal(t, core.CodecH264, p.GetMedias()[0].Codecs[0].Name)
	require.Equal(t, core.CodecAAC, p.GetMedias()[1].Codecs[0].Name)

	get, stopConsumers := consume(t, p)

	started := time.Now()
	errCh := make(chan error)
	go func() { errCh <- p.Start() }()

	// 4 seconds of video (1..5) with speed 20 = 0.2 seconds
	require.Eventually(t, func() bool { return checkVideo(t, get()) == 4*25 }, 3*time.Second, 10*time.Millisecond)
	require.Greater(t, time.Since(started), 150*time.Millisecond, "should be paced")

	// the player waits for Stop after the end, it doesn't restart
	select {
	case <-errCh:
		t.Fatal("player should not exit before Stop")
	case <-time.After(100 * time.Millisecond):
	}

	stopConsumers()
	require.NoError(t, p.Stop())
	require.NoError(t, <-errCh)

	// audio is continuous
	var audio []uint32
	for _, pkt := range get() {
		if pkt.kind == core.KindAudio {
			audio = append(audio, pkt.ts)
		}
	}
	require.Greater(t, len(audio), 50)
	for i := 1; i < len(audio); i++ {
		// ±1 tick on segment boundaries: offsets are rounded to the audio clock
		require.InDelta(t, 1024, int64(audio[i]-audio[i-1]), 1, "audio %d", i)
	}
}

func TestPlayerNoRecording(t *testing.T) {
	var mu sync.Mutex
	_, err := NewPlayer(PlayerOptions{
		From: time.Now(), To: time.Now().Add(time.Minute),
		List: listFunc(&mu, func() []Segment { return nil }),
	})
	require.ErrorIs(t, err, ErrNoRecording)
}

// TestPlayerLive follows the recording: plays segments that are recorded after the player start
func TestPlayerLive(t *testing.T) {
	var mu sync.Mutex
	segments := map[string]Segment{}

	cons := NewConsumer(nil, Options{
		Stream: "cam1", Dir: t.TempDir(), SegmentDuration: time.Second, MaxDrift: time.Hour,
		OnSegment: func(seg Segment) {
			mu.Lock()
			segments[seg.ID] = seg
			mu.Unlock()
		},
	})

	video := &core.Media{Kind: core.KindVideo, Direction: core.DirectionRecvonly}
	codec := &core.Codec{Name: core.CodecH264, ClockRate: 90000, PayloadType: core.PayloadTypeRAW}
	video.Codecs = []*core.Codec{codec}
	recv := core.NewReceiver(video, codec)
	require.NoError(t, cons.AddTrack(video, codec, recv))
	cons.Start()

	tr := &testRecorder{cons: cons, receiver: recv}

	// keyframe every second, segment every second: 2 seconds before the player start
	ts := tr.feed(0, 2*25)

	list := listFunc(&mu, func() []Segment {
		items := make([]Segment, 0, len(segments))
		for _, seg := range segments {
			items = append(items, seg)
		}
		return items
	})

	first := list(time.Time{}, time.Time{})[0]

	p, err := NewPlayer(PlayerOptions{From: first.Start, List: list}) // no end: live
	require.NoError(t, err)
	p.speed = 10

	get, stopConsumers := consume(t, p)
	errCh := make(chan error)
	go func() { errCh <- p.Start() }()

	// 3 more seconds recorded while playing (real time 1 frame per 20ms)
	for i := 0; i < 3*25; i++ {
		ts = tr.feed(ts, 1)
		time.Sleep(20 * time.Millisecond)
	}

	// everything except the data of the open segment that is not flushed yet
	require.Eventually(t, func() bool { return checkVideo(t, get()) >= 4*25 }, 5*time.Second, 20*time.Millisecond)

	stopConsumers()
	require.NoError(t, p.Stop())
	require.NoError(t, <-errCh)
	_ = cons.Stop()

	checkVideo(t, get())
}

func TestPlayerPCMA(t *testing.T) {
	// G.711 roundtrip through 16 bit PCM is lossless
	for i := 0; i < 256; i++ {
		require.Equal(t, byte(i), pcm.PCMtoPCMA(pcm.PCMAtoPCM(byte(i))), "alaw %d", i)
	}

	r := newTestRecorder(t, t.TempDir())

	audio := &core.Media{Kind: core.KindAudio, Direction: core.DirectionRecvonly}
	codec := &core.Codec{Name: core.CodecPCMA, ClockRate: 8000, Channels: 1, PayloadType: core.PayloadTypeRAW}
	audio.Codecs = []*core.Codec{codec}
	receiver := core.NewReceiver(audio, codec)
	r.cons.mu.Lock()
	r.cons.ready = false
	r.cons.mu.Unlock()
	require.NoError(t, r.cons.AddTrack(audio, codec, receiver))
	r.cons.Start()

	// 20 ms G.711 packets with all byte values
	var sent [][]byte
	var vts, ats uint32
	for i := 0; i < 50; i++ {
		nalu := []byte{h264.NALUTypePFrame | 0x40, 5}
		if i%25 == 0 {
			nalu = []byte{h264.NALUTypeIFrame | 0x60, 1}
		}
		r.receiver.WriteRTP(&rtp.Packet{Header: rtp.Header{Timestamp: vts}, Payload: h264.JoinNALU(nalu)})
		vts += 3600
		for float64(ats)/8000 < float64(vts)/90000 {
			payload := make([]byte, 160)
			for j := range payload {
				payload[j] = byte(len(sent)*7 + j)
			}
			sent = append(sent, payload)
			receiver.WriteRTP(&rtp.Packet{Header: rtp.Header{Timestamp: ats}, Payload: payload})
			ats += 160
		}
		time.Sleep(time.Millisecond)
	}
	segments := r.stop()
	require.NotEmpty(t, segments)

	var mu sync.Mutex
	p, err := NewPlayer(PlayerOptions{
		From: segments[0].Start, To: segments[len(segments)-1].End().Add(time.Second),
		List: listFunc(&mu, func() []Segment { return segments }),
	})
	require.NoError(t, err)
	p.speed = 20

	// FLAC track is offered as PCMA, PCMU and L16
	media := p.GetMedias()[1]
	require.Equal(t, []string{core.CodecPCMA, core.CodecPCMU, core.CodecPCM}, []string{media.Codecs[0].Name, media.Codecs[1].Name, media.Codecs[2].Name})

	track, err := p.GetTrack(media, media.Codecs[0])
	require.NoError(t, err)
	var got [][]byte
	sender := core.NewSender(media, media.Codecs[0])
	sender.Handler = func(pkt *core.Packet) {
		mu.Lock()
		got = append(got, pkt.Payload)
		mu.Unlock()
	}
	sender.HandleRTP(track)

	go func() { _ = p.Start() }()
	// audio packets before the first keyframe can be dropped by the recorder (separate goroutines)
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) >= len(sent)-5
	}, 3*time.Second, 10*time.Millisecond)
	time.Sleep(100 * time.Millisecond)
	_ = p.Stop()

	mu.Lock()
	defer mu.Unlock()
	k := 0
	for k < len(sent) && !bytes.Equal(sent[k], got[0]) {
		k++
	}
	require.Less(t, k, 5, "first packet should be one of the first sent")
	require.LessOrEqual(t, len(got), len(sent)-k)
	for i := range got {
		require.Equal(t, sent[k+i], got[i], "packet %d", i)
	}
}
