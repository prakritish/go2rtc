package record

import (
	"encoding/binary"
	"errors"
	"io"
	"os"

	"github.com/AlexxIT/go2rtc/pkg/iso"
)

// fragment - one moof+mdat pair inside a segment file
type fragment struct {
	offset   int64 // moof position in the file
	end      int64 // mdat end position in the file
	trackID  uint32
	seq      uint32
	dts      uint64
	duration uint32 // default sample duration (one sample per fragment)
	cts      int32  // composition time offset of the first sample
	flags    uint32 // default sample flags (keyframe or not)
	seqPos   int64  // mfhd sequence number position in the file
	dtsPos   int64  // tfdt decode time position in the file
	dtsV1    bool   // tfdt version 1 (uint64) or 0 (uint32)
}

const maxMoofSize = 64 * 1024

// scanFragments walks complete moof+mdat pairs starting from the offset until limit.
// It calls fn for each fragment, stops when fn returns false.
// Returns the end position of the last complete fragment.
// Incomplete data at the end of the file (crash or segment being written) is ignored.
func scanFragments(f *os.File, offset, limit int64, fn func(fr *fragment) bool) (int64, error) {
	header := make([]byte, 8)
	moof := make([]byte, 0, 1024)

	for offset < limit {
		moofSize, name, err := readBoxHeader(f, header, offset)
		if err != nil {
			return offset, nilOnEOF(err)
		}
		if name != iso.Moof || moofSize > maxMoofSize || offset+moofSize > limit {
			return offset, nil
		}

		moof = moof[:moofSize]
		if _, err = f.ReadAt(moof, offset); err != nil {
			return offset, nilOnEOF(err)
		}

		mdatPos := offset + moofSize
		mdatSize, name, err := readBoxHeader(f, header, mdatPos)
		if err != nil {
			return offset, nilOnEOF(err)
		}
		if name != iso.Mdat || mdatPos+mdatSize > limit {
			return offset, nil
		}

		fr := &fragment{offset: offset, end: mdatPos + mdatSize}
		if err = parseMoof(moof, fr); err != nil {
			return offset, err
		}

		offset = fr.end

		if !fn(fr) {
			break
		}
	}

	return offset, nil
}

func readBoxHeader(f *os.File, b []byte, offset int64) (int64, string, error) {
	if _, err := f.ReadAt(b, offset); err != nil {
		return 0, "", err
	}
	size := int64(binary.BigEndian.Uint32(b))
	if size < 8 {
		return 0, "", errors.New("record: unsupported box size")
	}
	return size, string(b[4:8]), nil
}

// parseMoof finds sequence number, track ID and decode time positions
func parseMoof(b []byte, fr *fragment) error {
	var hasSeq, hasDts bool

	walkBoxes(b[8:], 8, func(name string, data []byte, pos int) {
		switch name {
		case iso.MoofMfhd:
			if len(data) >= 8 {
				fr.seq = binary.BigEndian.Uint32(data[4:])
				fr.seqPos = fr.offset + int64(pos+8+4)
				hasSeq = true
			}
		case iso.MoofTraf:
			walkBoxes(data, pos+8, func(name string, data []byte, pos int) {
				switch name {
				case iso.MoofTrafTfhd:
					if len(data) >= 8 {
						fr.trackID = binary.BigEndian.Uint32(data[4:])
						fr.duration, fr.flags = tfhdDefaults(data)
					}
				case iso.MoofTrafTrun:
					fr.cts = trunCTS(data)
				case iso.MoofTrafTfdt:
					if len(data) >= 12 && data[0] == 1 {
						fr.dts = binary.BigEndian.Uint64(data[4:])
						fr.dtsV1 = true
					} else if len(data) >= 8 {
						fr.dts = uint64(binary.BigEndian.Uint32(data[4:]))
					} else {
						return
					}
					fr.dtsPos = fr.offset + int64(pos+8+4)
					hasDts = true
				}
			})
		}
	})

	if !hasSeq || !hasDts || fr.trackID == 0 {
		return errors.New("record: unsupported moof")
	}
	return nil
}

// tfhdDefaults returns default sample duration and flags from tfhd box data
func tfhdDefaults(data []byte) (duration, flags uint32) {
	tf := uint32(data[1])<<16 | uint32(data[2])<<8 | uint32(data[3])
	i := 8 // version + flags + track ID
	if tf&0x01 != 0 {
		i += 8 // base data offset
	}
	if tf&0x02 != 0 {
		i += 4 // sample description index
	}
	if tf&iso.TfhdDefaultSampleDuration != 0 {
		if len(data) >= i+4 {
			duration = binary.BigEndian.Uint32(data[i:])
		}
		i += 4
	}
	if tf&iso.TfhdDefaultSampleSize != 0 {
		i += 4
	}
	if tf&iso.TfhdDefaultSampleFlags != 0 && len(data) >= i+4 {
		flags = binary.BigEndian.Uint32(data[i:])
	}
	return
}

// trunCTS returns composition time offset of the first sample from trun box data
func trunCTS(data []byte) int32 {
	if len(data) < 8 {
		return 0
	}
	flags := uint32(data[1])<<16 | uint32(data[2])<<8 | uint32(data[3])
	if flags&iso.TrunSampleCTS == 0 {
		return 0
	}
	i := 8 // version + flags + sample count
	for _, f := range []uint32{iso.TrunDataOffset, iso.TrunFirstSampleFlags, iso.TrunSampleDuration, iso.TrunSampleSize, iso.TrunSampleFlags} {
		if flags&f != 0 {
			i += 4
		}
	}
	if len(data) < i+4 {
		return 0
	}
	return int32(binary.BigEndian.Uint32(data[i:]))
}

// walkBoxes calls fn for each child box, pos - box position relative to the parent start
func walkBoxes(b []byte, pos int, fn func(name string, data []byte, pos int)) {
	for len(b) >= 8 {
		size := int(binary.BigEndian.Uint32(b))
		if size < 8 || size > len(b) {
			return
		}
		fn(string(b[4:8]), b[8:size], pos)
		b = b[size:]
		pos += size
	}
}

func nilOnEOF(err error) error {
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		return nil
	}
	return err
}

// trackInfo - timescale and kind for each track from init segment
type trackInfo struct {
	timescale uint32
	video     bool
}

func parseInit(init []byte) (tracks map[uint32]*trackInfo, master uint32, err error) {
	atoms, err := iso.DecodeAtoms(init)
	if err != nil {
		return nil, 0, err
	}

	tracks = map[uint32]*trackInfo{}

	var trackID uint32
	for _, atom := range atoms {
		switch atom := atom.(type) {
		case *iso.AtomTkhd:
			trackID = atom.TrackID
			tracks[trackID] = &trackInfo{}
			if master == 0 {
				master = trackID
			}
		case *iso.AtomMdhd:
			if track := tracks[trackID]; track != nil {
				track.timescale = atom.TimeScale
			}
		case *iso.AtomVideo:
			if track := tracks[trackID]; track != nil {
				if !track.video && !tracks[master].video {
					master = trackID
				}
				track.video = true
			}
		}
	}

	for _, track := range tracks {
		if track.timescale == 0 {
			return nil, 0, errors.New("record: wrong init segment")
		}
	}
	if len(tracks) == 0 {
		return nil, 0, errors.New("record: no tracks in init segment")
	}

	return tracks, master, nil
}
