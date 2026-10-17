package record

import (
	"encoding/binary"
	"testing"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

// aacPacket makes RFC 3640 (AAC-hbr) packet with units of 3 bytes
func aacPacket(ts uint32, units int) *rtp.Packet {
	b := make([]byte, 2+units*2+units*3)
	binary.BigEndian.PutUint16(b, uint16(units*16)) // headers size in bits
	for i := 0; i < units; i++ {
		binary.BigEndian.PutUint16(b[2+i*2:], 3<<3) // unit size 3
		copy(b[2+units*2+i*3:], []byte{0x21, byte(i), 0})
	}
	return &rtp.Packet{Header: rtp.Header{Timestamp: ts}, Payload: b}
}

func TestAACDepay(t *testing.T) {
	var got []uint32
	d := &aacDepay{handler: func(p *core.Packet) { got = append(got, p.Timestamp) }}

	d.Handle(aacPacket(5000, 2))        // 5000, 6024
	d.Handle(aacPacket(5000+2048, 1))   // 7048
	d.Handle(aacPacket(5000+3072*2, 1)) // lost 2 units: 11144 (keeps RTP time)
	require.Equal(t, []uint32{5000, 6024, 7048, 11144}, got)
	require.False(t, d.broken.Load())

	// broken camera: timestamps don't follow units
	got = got[:0]
	for i := 0; i < 4; i++ {
		d.Handle(aacPacket(uint32(100+i*10), 1))
	}
	require.True(t, d.broken.Load())
	// own timestamps continue from the last good one (11144 + 1024)
	require.Equal(t, []uint32{12168, 13192, 14216, 15240}, got)

	// after the fallback good timestamps are not used anymore
	d.Handle(aacPacket(16264, 1))
	require.Equal(t, uint32(16264), got[4])
	d.Handle(aacPacket(90000, 1))
	require.Equal(t, uint32(17288), got[5])
}

func TestAACDepayReset(t *testing.T) {
	var got []uint32
	d := &aacDepay{handler: func(p *core.Packet) { got = append(got, p.Timestamp) }}

	d.Handle(aacPacket(5000, 1))
	d.reset() // reconnect: new session with new timestamps
	d.Handle(aacPacket(9_000_000, 1))
	d.Handle(aacPacket(9_000_000+1024, 1))
	require.Equal(t, []uint32{5000, 9_000_000, 9_001_024}, got)
	require.False(t, d.broken.Load())
}
