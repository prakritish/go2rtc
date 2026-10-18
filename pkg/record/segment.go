package record

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// Segment - info about one recorded file
type Segment struct {
	Stream    string        `json:"-"`
	ID        string        `json:"id"`
	Start     time.Time     `json:"start"`
	Duration  time.Duration `json:"-"`
	Size      int64         `json:"size"`
	Codecs    string        `json:"codecs,omitempty"`
	Recording bool          `json:"recording,omitempty"`
	Path      string        `json:"-"`
}

func (s *Segment) End() time.Time {
	return s.Start.Add(s.Duration)
}

// segmentWriter writes fMP4 data and keyframe index for one segment
type segmentWriter struct {
	info  Segment
	f     *os.File
	wr    *bufio.Writer
	idx   *indexWriter
	size  int64
	dirty bool

	correction time.Duration // start time correction relative to the file name

	// memory mode (events mode pre-buffer): keyframe intervals are kept in memory until materialize
	mem         bool
	memInit     []byte
	memGOPs     []*memGOP
	memPre      time.Duration // keep at least this duration
	memClosed   bool
	memDuration time.Duration
	timeBase    time.Duration // media time of the first written keyframe (after materialize)
}

type memGOP struct {
	t    time.Duration // keyframe time in the segment
	data []byte
}

// createMemSegment creates the segment in memory, it's not visible until materialize
func createMemSegment(stream string, start time.Time, init []byte, pre time.Duration) *segmentWriter {
	return &segmentWriter{
		info:    Segment{Stream: stream, ID: SegmentID(start), Start: start},
		mem:     true,
		memInit: init,
		memPre:  pre,
	}
}

// materialize writes the memory segment to the file, the segment starts from the first kept keyframe.
// The segment continues in the file mode (or it's closed if it was closed in the memory).
func (s *segmentWriter) materialize(dir string) error {
	if !s.mem || len(s.memGOPs) == 0 {
		return errors.New("record: nothing to materialize")
	}

	first := s.memGOPs[0].t
	start := s.info.Start.Add(first).Truncate(time.Millisecond)

	w, err := createSegment(dir, s.info.Stream, start, s.memInit)
	if err != nil {
		return err
	}
	w.info.Codecs = s.info.Codecs

	for _, gop := range s.memGOPs {
		n := gop.keyframe()
		if err = w.writeKeyframe(gop.data[:n], gop.t-first); err == nil {
			err = w.write(gop.data[n:])
		}
		if err != nil {
			_ = w.close(0)
			return err
		}
	}

	closed, duration := s.memClosed, s.memDuration
	*s = *w
	s.timeBase = first

	if closed {
		return s.close(duration)
	}
	return nil
}

// keyframe returns the size of the first fragment (keyframe moof + mdat) in the GOP data
func (g *memGOP) keyframe() int {
	moof := int(binary.BigEndian.Uint32(g.data))
	mdat := int(binary.BigEndian.Uint32(g.data[moof:]))
	return moof + mdat
}

// trimTo removes keyframe intervals before the one that contains the time t
func (s *segmentWriter) trimTo(t time.Duration) {
	i := 0
	for i+1 < len(s.memGOPs) && s.memGOPs[i+1].t <= t {
		i++
	}
	s.memGOPs = append([]*memGOP(nil), s.memGOPs[i:]...)
}

func createSegment(dir, stream string, start time.Time, init []byte) (*segmentWriter, error) {
	id := SegmentID(start)
	path := filepath.Join(dir, filepath.FromSlash(id))

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, err
	}

	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}

	idx, err := createIndex(IndexPath(path))
	if err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return nil, err
	}

	s := &segmentWriter{
		info: Segment{
			Stream: stream, ID: id, Start: start, Path: path, Recording: true,
		},
		f:   f,
		wr:  bufio.NewWriterSize(f, 512*1024),
		idx: idx,
	}

	if err = s.write(init); err != nil {
		_ = s.close(0)
		return nil, err
	}

	return s, nil
}

func (s *segmentWriter) write(b []byte) error {
	if s.mem {
		if n := len(s.memGOPs); n > 0 {
			s.memGOPs[n-1].data = append(s.memGOPs[n-1].data, b...)
		}
		return nil
	}
	n, err := s.wr.Write(b)
	s.size += int64(n)
	s.dirty = true
	return err
}

// writeKeyframe writes keyframe data, flushes file buffer and adds index entry.
// So the index never points to the data that is not written to the file.
func (s *segmentWriter) writeKeyframe(b []byte, ts time.Duration) error {
	if s.mem {
		s.memGOPs = append(s.memGOPs, &memGOP{t: ts, data: append([]byte(nil), b...)})
		// keep the minimum number of keyframe intervals that covers the pre time
		if len(s.memGOPs) > 1 && s.memGOPs[1].t <= ts-s.memPre {
			s.trimTo(ts - s.memPre)
		}
		return nil
	}

	ts -= s.timeBase
	offset := s.size
	if err := s.write(b); err != nil {
		return err
	}
	if err := s.flush(); err != nil {
		return err
	}
	return s.idx.Write(IndexEntry{Type: EntryKeyframe, Time: ts, Offset: offset})
}

func (s *segmentWriter) flush() error {
	if s.mem || !s.dirty {
		return nil
	}
	s.dirty = false
	return s.wr.Flush()
}

func (s *segmentWriter) close(duration time.Duration) error {
	if s.mem {
		s.memClosed = true
		s.memDuration = duration
		return nil
	}

	duration -= s.timeBase

	err := s.flush()
	if err2 := s.f.Sync(); err == nil {
		err = err2
	}
	if err2 := s.f.Close(); err == nil {
		err = err2
	}

	if err == nil && s.correction != 0 {
		err = s.idx.Write(IndexEntry{Type: EntryStart, Offset: int64(s.correction / time.Millisecond)})
	}
	if err == nil {
		err = s.idx.Write(IndexEntry{Type: EntryEnd, Time: duration, Offset: s.size})
	}
	if err2 := s.idx.Close(); err == nil {
		err = err2
	}

	s.info.Duration = duration
	s.info.Size = s.size
	s.info.Recording = false

	return err
}

// setStart corrects the segment start time, the file name stays the same,
// the correction is saved to the index on close
func (s *segmentWriter) setStart(start time.Time) {
	name, _ := ParseSegmentID(s.info.ID)
	s.correction = start.Sub(name)
	s.info.Start = start
}

func (s Segment) MarshalJSON() ([]byte, error) {
	type alias Segment
	return json.Marshal(struct {
		alias
		End      time.Time `json:"end"`
		Duration float64   `json:"duration"`
	}{
		alias:    alias(s),
		End:      s.End(),
		Duration: s.Duration.Seconds(),
	})
}
