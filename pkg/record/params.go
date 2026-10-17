package record

import (
	"bytes"
	"encoding/binary"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/h264"
	"github.com/AlexxIT/go2rtc/pkg/h265"
)

// updateParams checks in-band parameter sets of the keyframe (AVCC) and updates the codec.
// Returns true if the parameter sets were changed (not the first time).
func updateParams(codec *core.Codec, avcc []byte) bool {
	switch codec.Name {
	case core.CodecH264:
		var sps, pps []byte
		walkNALU(avcc, func(nalu []byte) {
			switch nalu[0] & 0x1F {
			case h264.NALUTypeSPS:
				sps = nalu
			case h264.NALUTypePPS:
				pps = nalu
			}
		})
		if sps == nil || pps == nil {
			return false
		}
		oldSPS, oldPPS := h264.GetParameterSet(codec.FmtpLine)
		if bytes.Equal(sps, oldSPS) && bytes.Equal(pps, oldPPS) {
			return false
		}
		codec.FmtpLine = h264.AVCCToCodec(h264.JoinNALU(sps, pps)).FmtpLine
		return oldSPS != nil

	case core.CodecH265:
		var vps, sps, pps []byte
		walkNALU(avcc, func(nalu []byte) {
			switch (nalu[0] >> 1) & 0x3F {
			case h265.NALUTypeVPS:
				vps = nalu
			case h265.NALUTypeSPS:
				sps = nalu
			case h265.NALUTypePPS:
				pps = nalu
			}
		})
		if vps == nil || sps == nil || pps == nil {
			return false
		}
		oldVPS, oldSPS, oldPPS := h265.GetParameterSet(codec.FmtpLine)
		if bytes.Equal(vps, oldVPS) && bytes.Equal(sps, oldSPS) && bytes.Equal(pps, oldPPS) {
			return false
		}
		codec.FmtpLine = h265.AVCCToCodec(h264.JoinNALU(vps, sps, pps)).FmtpLine
		return oldSPS != nil
	}
	return false
}

// walkNALU calls fn for each NAL unit (without length prefix) in AVCC data
func walkNALU(avcc []byte, fn func(nalu []byte)) {
	for len(avcc) > 4 {
		size := int(binary.BigEndian.Uint32(avcc)) + 4
		if size <= 4 || size > len(avcc) {
			return
		}
		fn(avcc[4:size])
		avcc = avcc[size:]
	}
}
