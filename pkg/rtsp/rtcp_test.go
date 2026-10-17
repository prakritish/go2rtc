package rtsp

import (
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/pion/rtcp"
	"github.com/stretchr/testify/require"
)

func TestSenderReport(t *testing.T) {
	media := &core.Media{Kind: core.KindAudio}
	codec := &core.Codec{Name: core.CodecPCMA, ClockRate: 8000}
	receiver := core.NewReceiver(media, codec)
	receiver.ID = 2 // RTP channel, RTCP channel = 3

	c := &Conn{}
	c.Receivers = append(c.Receivers, receiver)

	// 2026-01-01 00:00:00.5 UTC in NTP format
	ntpTime := uint64(2208988800+1767225600)<<32 | 1<<31
	sr := &rtcp.SenderReport{SSRC: 1, NTPTime: ntpTime, RTPTime: 123456}
	b, err := sr.Marshal()
	require.NoError(t, err)

	require.NoError(t, c.handleRawPacket(3, b))

	got := receiver.SenderReport()
	require.NotNil(t, got)
	require.Equal(t, uint32(123456), got.RTPTime)
	require.True(t, got.NTP.Equal(time.Date(2026, 1, 1, 0, 0, 0, 5e8, time.UTC)), got.NTP.String())

	// RTP time to the sender time
	require.True(t, got.Time(123456+8000, 8000).Equal(got.NTP.Add(time.Second)))
	require.True(t, got.Time(123456-4000, 8000).Equal(got.NTP.Add(-500*time.Millisecond)))

	// broken RTCP is ignored
	require.NoError(t, c.handleRawPacket(3, []byte{0x80, 200, 0, 1}))
}
