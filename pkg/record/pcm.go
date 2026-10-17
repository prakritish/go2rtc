package record

import (
	"encoding/binary"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/pcm"
)

// pcmCodecs - codecs for playback of FLAC (PCM) track
func pcmCodecs(clockRate uint32) []*core.Codec {
	l16 := &core.Codec{Name: core.CodecPCM, ClockRate: clockRate, Channels: 1, PayloadType: core.PayloadTypeRAW}
	if clockRate != 8000 {
		return []*core.Codec{l16}
	}
	// most likely the camera had G.711, it's supported by WebRTC and RTSP clients
	return []*core.Codec{
		{Name: core.CodecPCMA, ClockRate: 8000, Channels: 1, PayloadType: core.PayloadTypeRAW},
		{Name: core.CodecPCMU, ClockRate: 8000, Channels: 1, PayloadType: core.PayloadTypeRAW},
		l16,
	}
}

// decodeFLAC returns 16 bit big endian samples from the FLAC frame made by pcm.FLACEncoder
// (one channel, one verbatim subframe). Returns nil for other frames.
func decodeFLAC(frame []byte) []byte {
	// sync code 0xFFF8 + variable block size, block size type 7 (16 bit value at the end of the header)
	if len(frame) < 10 || frame[0] != 0xFF || frame[1]&0xFE != 0xF8 || frame[2]>>4 != 7 || frame[3]>>4 != 0 {
		return nil
	}

	// UTF-8 coded sample number
	n := 4
	switch b := frame[4]; {
	case b < 0x80:
		n += 1
	case b < 0xE0:
		n += 2
	case b < 0xF0:
		n += 3
	case b < 0xF8:
		n += 4
	default:
		return nil
	}
	if len(frame) < n+4 {
		return nil
	}

	samples := int(binary.BigEndian.Uint16(frame[n:])) + 1
	n += 2 + 1 // block size + CRC8

	if frame[n]&0x7E != 0x02 { // verbatim subframe
		return nil
	}
	n++

	if len(frame) < n+samples*2+2 {
		return nil
	}
	return frame[n : n+samples*2]
}

// encodePCM converts 16 bit big endian samples to the codec
func encodePCM(codecName string, samples []byte) []byte {
	switch codecName {
	case core.CodecPCMA, core.CodecPCMU:
		b := make([]byte, len(samples)/2)
		for i := range b {
			s16 := int16(binary.BigEndian.Uint16(samples[i*2:]))
			if codecName == core.CodecPCMA {
				b[i] = pcm.PCMtoPCMA(s16)
			} else {
				b[i] = pcm.PCMtoPCMU(s16)
			}
		}
		return b
	}
	return samples // L16 is big endian
}
