package record

import (
	"errors"
	"os"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/iso"
)

// RepairSegment rebuilds the index of the segment from the MP4 file and truncates incomplete
// data at the end of the file. It's used after a crash, when the index is missing or has no end entry.
// Returns the segment duration.
func RepairSegment(path string) (time.Duration, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return 0, err
	}

	// init segment: boxes before the first moof
	header := make([]byte, 8)
	var initSize int64
	for {
		size, name, err := readBoxHeader(f, header, initSize)
		if err != nil {
			return 0, err
		}
		if name == iso.Moof {
			break
		}
		if name != iso.Ftyp && name != iso.Moov {
			return 0, errors.New("record: unknown box " + name)
		}
		initSize += size
	}

	init := make([]byte, initSize)
	if _, err = f.ReadAt(init, 0); err != nil {
		return 0, err
	}

	tracks, master, err := parseInit(init)
	if err != nil {
		return 0, err
	}
	timescale := uint64(tracks[master].timescale)
	video := tracks[master].video

	var entries []IndexEntry
	var firstDts, masterEnd uint64
	var started bool

	end, err := scanFragments(f, initSize, fi.Size(), func(fr *fragment) bool {
		if fr.trackID != master {
			return true
		}
		if !started {
			firstDts = fr.dts
			started = true
		}
		masterEnd = fr.dts + uint64(fr.duration)

		t := time.Duration((fr.dts-firstDts)*1000/timescale) * time.Millisecond

		var keyframe bool
		if video {
			keyframe = fr.flags == iso.SampleVideoIFrame
		} else {
			// audio only: index entry every second, like the recorder
			keyframe = len(entries) == 0 || t-entries[len(entries)-1].Time >= time.Second
		}
		if keyframe {
			entries = append(entries, IndexEntry{Type: EntryKeyframe, Time: t, Offset: fr.offset})
		}
		return true
	})
	if err != nil {
		return 0, err
	}
	if len(entries) == 0 {
		return 0, errors.New("record: no keyframes in segment")
	}

	// incomplete fragment at the end of the file
	if end < fi.Size() {
		if err = f.Truncate(end); err != nil {
			return 0, err
		}
	}

	duration := time.Duration((masterEnd-firstDts)*1000/timescale) * time.Millisecond
	entries = append(entries, IndexEntry{Type: EntryEnd, Time: duration, Offset: end})

	b := []byte(indexMagic)
	for _, e := range entries {
		b = appendIndexEntry(b, e)
	}

	// atomic write
	idxPath := IndexPath(path)
	if err = os.WriteFile(idxPath+".tmp", b, 0644); err != nil {
		return 0, err
	}
	if err = os.Rename(idxPath+".tmp", idxPath); err != nil {
		return 0, err
	}

	return duration, nil
}
