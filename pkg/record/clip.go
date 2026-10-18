package record

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"os"
	"sort"
	"time"
)

// ErrNoRecording - no recorded data for the requested time range
var ErrNoRecording = errors.New("record: no recording for the time range")

// clipGapTolerance - distance between segments that is not counted as a gap (start time inaccuracy)
const clipGapTolerance = time.Second

// Clip - virtual fMP4 file made from parts of several segments without transcoding.
// The clip is init segment + fragments with rewritten sequence numbers and decode times,
// so the size is known in advance and the clip supports random access (HTTP Range).
type Clip struct {
	Start    time.Time     // real start (keyframe at or before the requested start)
	End      time.Time     // real end
	Duration time.Duration // media duration (without gaps), including Preroll
	Preroll  time.Duration // media time before the start keyframe (late audio samples), video starts here
	Gaps     int           // number of gaps (camera offline) inside the clip, they are skipped
	Codecs   string

	// Truncated - the clip ends earlier than requested because codec parameters changed
	Truncated bool

	Keyframes []ClipKeyframe

	init   []byte
	parts  []*clipPart
	size   int64
	tracks map[uint32]*trackInfo
	master uint32
}

type clipPart struct {
	path     string
	from, to int64 // byte range in the segment file
	offset   int64 // position in the clip
	seqBase  uint32
	shift    map[uint32]int64 // decode time shift for each track
}

// ClipKeyframe - keyframe position in the clip, useful for HLS playlist
type ClipKeyframe struct {
	Offset int64         // byte position in the clip
	Time   time.Duration // media time in the clip
	Real   time.Time     // real time
}

type patch struct {
	pos int64 // position in the segment file
	b   [8]byte
	n   int
}

// NewClip builds a clip for [from, to) from the sorted list of segments
func NewClip(segments []Segment, from, to time.Time) (*Clip, error) {
	if !from.Before(to) {
		return nil, errors.New("record: wrong time range")
	}

	c := &Clip{}

	// 1. Plan parts
	var planned []*plannedPart
	for _, seg := range segments {
		if !seg.Start.Before(to) {
			break
		}

		p, err := c.planPart(seg, from, to, len(planned))
		if err != nil {
			return nil, err
		}
		if p == nil {
			if c.Truncated {
				break
			}
			continue
		}
		planned = append(planned, p)
	}

	// 2. Cut secondary track samples (audio) at the end of the part that overlap with the next part.
	// For example, the camera sends audio for some time after video when the connection breaks.
	for i := 0; i+1 < len(planned); i++ {
		p, next := planned[i], planned[i+1]
		limits := map[uint32]float64{}
		for id, first := range next.firstDts {
			// next part sample time in this part decode time
			limits[id] = p.base + p.duration + first - next.base
		}
		if err := c.cutTail(p, limits); err != nil {
			return nil, err
		}
	}

	// 3. Place parts in the clip
	var prevEnd time.Time
	var accum float64 // clip duration in seconds before the current part

	for _, p := range planned {
		if c.parts == nil {
			c.Start = p.start
			// the first part may have audio samples before the start keyframe
			accum = p.preroll
			c.Preroll = time.Duration(p.preroll * float64(time.Second))
		} else if p.start.Sub(prevEnd) > clipGapTolerance {
			c.Gaps++
		}

		part := p.part
		part.offset = int64(len(c.init)) + c.size
		part.seqBase = uint32(len(c.parts)+1) << 20

		part.shift = make(map[uint32]int64, len(c.tracks))
		for id, track := range c.tracks {
			ts := float64(track.timescale)
			part.shift[id] = int64(math.Round(accum*ts)) - int64(math.Round(p.base*ts))
		}

		for _, kf := range p.keyframes {
			if kf.offset >= part.to {
				continue
			}
			c.Keyframes = append(c.Keyframes, ClipKeyframe{
				Offset: part.offset + kf.offset - part.from,
				Time:   time.Duration((accum + kf.time) * float64(time.Second)),
				Real:   kf.real,
			})
		}

		accum += p.duration
		prevEnd = p.start.Add(time.Duration(p.duration * float64(time.Second)))

		c.size += part.to - part.from
		c.parts = append(c.parts, part)
		c.End = prevEnd
	}

	if c.parts == nil {
		return nil, ErrNoRecording
	}

	c.size += int64(len(c.init))
	c.Duration = time.Duration(accum * float64(time.Second))

	return c, nil
}

type plannedPart struct {
	part     *clipPart
	start    time.Time // real time of the part start (from the index)
	base     float64   // master track decode time of the part start, seconds
	duration float64   // seconds
	preroll  float64   // seconds, secondary track samples before the start keyframe (only first part)

	firstDts     map[uint32]float64 // first decode time of secondary tracks (seconds)
	lastKeyframe int64              // offset of the last keyframe in the part

	keyframes []partKeyframe
}

type partKeyframe struct {
	offset int64     // position in the segment file
	time   float64   // seconds from the part start
	real   time.Time // real time
}

func (c *Clip) planPart(seg Segment, from, to time.Time, n int) (*plannedPart, error) {
	entries, err := ReadIndex(IndexPath(seg.Path))
	if err != nil {
		return nil, nil // segment without index, skip it
	}

	var keyframes []IndexEntry
	var end *IndexEntry
	for i, e := range entries {
		switch e.Type {
		case EntryKeyframe:
			keyframes = append(keyframes, e)
		case EntryEnd:
			end = &entries[i]
		}
	}
	if len(keyframes) == 0 {
		return nil, nil
	}

	f, err := os.Open(seg.Path)
	if err != nil {
		return nil, nil // segment can be removed by retention
	}
	defer f.Close()

	init := make([]byte, keyframes[0].Offset)
	if _, err = f.ReadAt(init, 0); err != nil {
		return nil, nil
	}

	if c.init == nil {
		if c.tracks, c.master, err = parseInit(init); err != nil {
			return nil, nil
		}
		c.init = init
		c.Codecs = seg.Codecs
	} else if !bytes.Equal(c.init, init) {
		// codec parameters changed, can't continue in one MP4 file
		c.Truncated = true
		return nil, nil
	}

	master := c.master
	timescale := float64(c.tracks[master].timescale)

	// 1. Find start keyframe
	relFrom := from.Sub(seg.Start)
	k := 0
	if n == 0 {
		k = sort.Search(len(keyframes), func(i int) bool {
			return keyframes[i].Time > relFrom
		}) - 1
		if k < 0 {
			k = 0
		}
	}

	partFrom := keyframes[k].Offset
	partStart := seg.Start.Add(keyframes[k].Time)

	// 2. Find end of data: end entry for closed segment, last complete fragment otherwise
	var fileEnd int64
	if end != nil {
		fileEnd = end.Offset
	} else {
		fi, err := f.Stat()
		if err != nil {
			return nil, nil
		}
		last := keyframes[len(keyframes)-1].Offset
		if fileEnd, err = scanFragments(f, last, fi.Size(), func(*fragment) bool { return true }); err != nil {
			return nil, err
		}
	}

	// 3. Read decode times (seconds) of the first keyframe in the segment and the start keyframe.
	// Master track can start with a lead (see fragmenter), index time is relative to the first keyframe.
	firstDts, ok := fragmentDts(f, keyframes[0].Offset, fileEnd, timescale)
	if !ok {
		return nil, nil
	}
	base, ok := fragmentDts(f, partFrom, fileEnd, timescale)
	if !ok {
		return nil, nil
	}

	// 4. Find precise end: first master frame at or after requested end
	relTo := to.Sub(seg.Start)
	partTo := fileEnd
	mediaEnd := -1.0

	k = sort.Search(len(keyframes), func(i int) bool {
		return keyframes[i].Time > relTo
	}) - 1
	if k >= 0 && keyframes[k].Offset < fileEnd {
		dtsTo := relTo.Seconds() + firstDts
		if _, err = scanFragments(f, keyframes[k].Offset, fileEnd, func(fr *fragment) bool {
			if fr.trackID != master {
				return true
			}
			if t := float64(fr.dts) / timescale; t >= dtsTo {
				mediaEnd = t
				partTo = fr.offset // this frame is not included
				return false
			}
			return true
		}); err != nil {
			return nil, err
		}
	}

	if mediaEnd < 0 {
		// whole segment till the end of the last master sample
		mediaEnd = masterEnd(f, keyframes[len(keyframes)-1].Offset, fileEnd, master, timescale)
	}

	if partTo <= partFrom || mediaEnd <= base {
		return nil, nil
	}

	// first part: segment ends before the requested start
	if n == 0 && mediaEnd <= relFrom.Seconds()+firstDts {
		return nil, nil
	}

	// First decode time of secondary tracks (first keyframe interval is enough).
	// Audio usually comes later than video, so the samples before the start keyframe
	// can be written after it.
	firsts := map[uint32]float64{}
	limit := partTo
	for _, kf := range keyframes {
		if kf.Offset > partFrom {
			limit = min(kf.Offset, partTo)
			break
		}
	}
	_, _ = scanFragments(f, partFrom, limit, func(fr *fragment) bool {
		if track := c.tracks[fr.trackID]; fr.trackID != master && track != nil {
			t := float64(fr.dts) / float64(track.timescale)
			if v, ok := firsts[fr.trackID]; !ok || t < v {
				firsts[fr.trackID] = t
			}
		}
		return true
	})

	// first part: how much earlier secondary samples are than the start keyframe
	var preroll float64
	if n == 0 {
		for _, t := range firsts {
			if d := base - t; d > preroll {
				preroll = min(d, maxLead.Seconds())
			}
		}
	}

	lastKeyframe := partFrom
	for _, kf := range keyframes {
		if kf.Offset >= partFrom && kf.Offset < partTo {
			lastKeyframe = kf.Offset
		}
	}

	var kfs []partKeyframe
	for _, kf := range keyframes {
		if kf.Offset >= partFrom && kf.Offset < partTo {
			kfs = append(kfs, partKeyframe{
				offset: kf.Offset,
				time:   kf.Time.Seconds() + firstDts - base,
				real:   seg.Start.Add(kf.Time),
			})
		}
	}

	return &plannedPart{
		part:      &clipPart{path: seg.Path, from: partFrom, to: partTo},
		start:     partStart,
		base:      base,
		duration:  mediaEnd - base,
		preroll:   preroll,
		keyframes: kfs,

		firstDts:     firsts,
		lastKeyframe: lastKeyframe,
	}, nil
}

// cutTail cuts secondary track samples at the end of the part with decode time (seconds) after the limit
func (c *Clip) cutTail(p *plannedPart, limits map[uint32]float64) error {
	f, err := os.Open(p.part.path)
	if err != nil {
		return nil // the part will fail on read
	}
	defer f.Close()

	cut := p.lastKeyframe
	if _, err = scanFragments(f, p.lastKeyframe, p.part.to, func(fr *fragment) bool {
		track := c.tracks[fr.trackID]
		limit, ok := limits[fr.trackID]
		if fr.trackID == c.master || track == nil || !ok || float64(fr.dts)/float64(track.timescale) < limit {
			cut = fr.end
		}
		return true
	}); err != nil {
		return err
	}
	if cut > p.part.from {
		p.part.to = cut
	}
	return nil
}

// fragmentDts returns decode time in seconds of the fragment at the offset
func fragmentDts(f *os.File, offset, end int64, timescale float64) (t float64, ok bool) {
	_, _ = scanFragments(f, offset, end, func(fr *fragment) bool {
		t, ok = float64(fr.dts)/timescale, true
		return false
	})
	return
}

// masterEnd returns the end time in seconds (decode time + duration) of the last master sample
func masterEnd(f *os.File, from, end int64, master uint32, timescale float64) (t float64) {
	_, _ = scanFragments(f, from, end, func(fr *fragment) bool {
		if fr.trackID == master {
			t = float64(fr.dts+uint64(fr.duration)) / timescale
		}
		return true
	})
	return
}

// InitSize - size of the init segment (ftyp + moov) at the start of the clip
func (c *Clip) InitSize() int64 {
	return int64(len(c.init))
}

func (c *Clip) Size() int64 {
	return c.size
}

// NewReader returns io.ReadSeeker for the clip, each reader has own file handles
func (c *Clip) NewReader() *ClipReader {
	return &ClipReader{clip: c}
}

// ClipReader - the clip is immutable, so many readers can use one clip concurrently
type ClipReader struct {
	clip *Clip
	pos  int64

	part    *clipPart
	f       *os.File
	patches []patch // for the current part
}

func (r *ClipReader) Read(p []byte) (n int, err error) {
	c := r.clip

	if r.pos >= c.size {
		return 0, io.EOF
	}

	// init segment
	if r.pos < int64(len(c.init)) {
		n = copy(p, c.init[r.pos:])
		r.pos += int64(n)
		return n, nil
	}

	i := sort.Search(len(c.parts), func(i int) bool {
		return c.parts[i].offset > r.pos
	}) - 1
	part := c.parts[i]

	if err = r.open(part); err != nil {
		return 0, err
	}

	filePos := part.from + (r.pos - part.offset)
	if left := part.to - filePos; int64(len(p)) > left {
		p = p[:left]
	}

	if n, err = r.f.ReadAt(p, filePos); err != nil {
		if err != io.EOF || n == 0 {
			return n, err
		}
		err = nil
	}

	applyPatches(r.patches, p[:n], filePos)

	r.pos += int64(n)
	return n, nil
}

func (r *ClipReader) open(part *clipPart) error {
	if r.part == part {
		return nil
	}

	if r.f != nil {
		_ = r.f.Close()
		r.f = nil
		r.part = nil
	}

	f, err := os.Open(part.path)
	if err != nil {
		return err
	}

	if r.patches, err = part.buildPatches(f, r.patches[:0]); err != nil {
		_ = f.Close()
		return err
	}

	r.f = f
	r.part = part
	return nil
}

func (r *ClipReader) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		offset += r.pos
	case io.SeekEnd:
		offset += r.clip.size
	default:
		return 0, errors.New("record: wrong whence")
	}
	if offset < 0 {
		return 0, errors.New("record: negative position")
	}
	r.pos = offset
	return offset, nil
}

func (r *ClipReader) Close() error {
	if r.f != nil {
		return r.f.Close()
	}
	return nil
}

// buildPatches calculates new sequence numbers and decode times for all fragments in the part
func (p *clipPart) buildPatches(f *os.File, patches []patch) ([]patch, error) {
	_, err := scanFragments(f, p.from, p.to, func(fr *fragment) bool {
		seq := patch{pos: fr.seqPos, n: 4}
		binary.BigEndian.PutUint32(seq.b[:], p.seqBase|fr.seq&0xFFFFF)

		dts := int64(fr.dts) + p.shift[fr.trackID]
		if dts < 0 {
			dts = 0
		}

		tfdt := patch{pos: fr.dtsPos}
		if fr.dtsV1 {
			tfdt.n = 8
			binary.BigEndian.PutUint64(tfdt.b[:], uint64(dts))
		} else {
			tfdt.n = 4
			binary.BigEndian.PutUint32(tfdt.b[:], uint32(dts))
		}

		patches = append(patches, seq, tfdt)
		return true
	})

	return patches, err
}

// apply patches to the buffer b that was read from the file position pos
func applyPatches(patches []patch, b []byte, pos int64) {
	end := pos + int64(len(b))

	// first patch that may overlap the buffer
	i := sort.Search(len(patches), func(i int) bool {
		return patches[i].pos+int64(patches[i].n) > pos
	})

	for ; i < len(patches); i++ {
		pt := &patches[i]
		if pt.pos >= end {
			break
		}
		for j := 0; j < pt.n; j++ {
			if k := pt.pos + int64(j) - pos; k >= 0 && k < int64(len(b)) {
				b[k] = pt.b[j]
			}
		}
	}
}
