package record

import (
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMemGOPKeyframe(t *testing.T) {
	box := func(size int) []byte {
		b := make([]byte, size)
		binary.BigEndian.PutUint32(b, uint32(size))
		return b
	}
	gop := func(parts ...[]byte) *memGOP {
		var data []byte
		for _, p := range parts {
			data = append(data, p...)
		}
		return &memGOP{data: data}
	}

	// moof + mdat, then the rest of the GOP
	require.Equal(t, 40, gop(box(16), box(24), box(30)).keyframe())

	// malformed data is written whole instead of a panic
	require.Equal(t, 3, gop([]byte{0, 0, 0}).keyframe())
	require.Equal(t, 16, gop(box(16)).keyframe())                      // no mdat
	require.Equal(t, 20, gop(box(16), []byte{0, 0, 0, 99}).keyframe()) // mdat past the end
	require.Equal(t, 12, gop([]byte{0xff, 0xff, 0xff, 0xff}, make([]byte, 8)).keyframe())
}
