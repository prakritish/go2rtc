package record

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/record"
)

// catalog - in-memory list of all segments, sorted by start time for each stream.
// It is rebuilt from the filesystem on startup, so the filesystem is the source of truth.
type catalog struct {
	root     string
	segments map[string][]*record.Segment
	mu       sync.RWMutex
}

func newCatalog(root string) *catalog {
	return &catalog{root: root, segments: map[string][]*record.Segment{}}
}

func (c *catalog) streamDir(stream string) string {
	return filepath.Join(c.root, record.EscapeName(stream))
}

// scan loads all segments from the filesystem
func (c *catalog) scan() error {
	dirs, err := os.ReadDir(c.root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	segments := map[string][]*record.Segment{}

	for _, dir := range dirs {
		// skip files and service dirs (.tmp), escaped stream names never start with a dot
		if !dir.IsDir() || strings.HasPrefix(dir.Name(), ".") {
			continue
		}

		stream, err := record.UnescapeName(dir.Name())
		if err != nil {
			continue
		}

		streamDir := filepath.Join(c.root, dir.Name())
		paths, _ := filepath.Glob(filepath.Join(streamDir, "*", "*"+record.ExtVideo))

		for _, path := range paths {
			if seg := loadSegment(stream, streamDir, path); seg != nil {
				segments[stream] = append(segments[stream], seg)
			}
		}
	}

	for _, items := range segments {
		sortSegments(items)
	}

	c.mu.Lock()
	c.segments = segments
	c.mu.Unlock()

	return nil
}

func loadSegment(stream, streamDir, path string) *record.Segment {
	start, ok := record.ParseSegmentID(path)
	if !ok {
		return nil
	}

	fi, err := os.Stat(path)
	if err != nil {
		return nil
	}

	id, err := filepath.Rel(streamDir, path)
	if err != nil {
		return nil
	}

	seg := &record.Segment{
		Stream: stream,
		ID:     filepath.ToSlash(id),
		Start:  start,
		Size:   fi.Size(),
		Path:   path,
	}

	var closed bool
	if entries, err := record.ReadIndex(record.IndexPath(path)); err == nil {
		seg.Duration, closed = record.IndexDuration(entries)
		seg.Start = seg.Start.Add(record.IndexStart(entries)) // corrected start time
	}

	// index without the end entry or without index (crash): rebuild it from the file,
	// it's safe on startup, because nothing is recorded yet
	if !closed {
		if duration, err := record.RepairSegment(path); err == nil {
			log.Info().Str("path", path).Msg("[record] segment repaired")
			seg.Duration = duration
			if fi, err = os.Stat(path); err == nil {
				seg.Size = fi.Size()
			}
		} else {
			log.Warn().Err(err).Str("path", path).Msg("[record] can't repair segment")
		}
	}

	return seg
}

// update adds a new segment or updates an existing one (by ID)
func (c *catalog) update(info record.Segment) {
	c.mu.Lock()
	defer c.mu.Unlock()

	items := c.segments[info.Stream]

	// most likely it is the last item
	for i := len(items) - 1; i >= 0; i-- {
		if items[i].ID == info.ID {
			*items[i] = info
			sortSegments(items)
			return
		}
	}

	c.segments[info.Stream] = append(items, &info)
	sortSegments(c.segments[info.Stream])
}

// list returns copies of segments that overlap [from, to). Zero time means no limit.
func (c *catalog) list(stream string, from, to time.Time) []record.Segment {
	c.mu.RLock()
	defer c.mu.RUnlock()

	items := c.segments[stream]
	result := make([]record.Segment, 0)

	for _, seg := range items {
		if !to.IsZero() && !seg.Start.Before(to) {
			break // sorted by start
		}
		if !from.IsZero() && !seg.End().After(from) && !seg.Recording {
			continue
		}
		result = append(result, *seg)
	}

	return result
}

func (c *catalog) get(stream, id string) *record.Segment {
	c.mu.RLock()
	defer c.mu.RUnlock()

	for _, seg := range c.segments[stream] {
		if seg.ID == id {
			info := *seg
			return &info
		}
	}
	return nil
}

func (c *catalog) streams() map[string]int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()

	sizes := make(map[string]int64, len(c.segments))
	for stream, items := range c.segments {
		var size int64
		for _, seg := range items {
			size += seg.Size
		}
		sizes[stream] = size
	}
	return sizes
}

func (c *catalog) totalSize() (size int64) {
	for _, n := range c.streams() {
		size += n
	}
	return
}

// remove deletes segments matching the filter from the catalog and from the disk.
// Segments that are being recorded are never removed.
func (c *catalog) remove(match func(seg *record.Segment) bool) (count int, size int64) {
	var removed []*record.Segment

	c.mu.Lock()
	for stream, items := range c.segments {
		keep := items[:0]
		for _, seg := range items {
			if !seg.Recording && match(seg) {
				removed = append(removed, seg)
			} else {
				keep = append(keep, seg)
			}
		}
		if len(keep) == 0 {
			delete(c.segments, stream)
		} else {
			c.segments[stream] = keep
		}
	}
	c.mu.Unlock()

	for _, seg := range removed {
		if err := os.Remove(seg.Path); err != nil && !os.IsNotExist(err) {
			log.Warn().Err(err).Str("path", seg.Path).Msg("[record] can't remove segment")
			continue
		}
		_ = os.Remove(record.IndexPath(seg.Path))
		_ = os.Remove(filepath.Dir(seg.Path)) // removes date dir only if empty

		count++
		size += seg.Size
	}

	return
}

// oldest returns the oldest segment start among all streams (not recording)
func (c *catalog) oldest() *record.Segment {
	c.mu.RLock()
	defer c.mu.RUnlock()

	var oldest *record.Segment
	for _, items := range c.segments {
		for _, seg := range items {
			if seg.Recording {
				continue
			}
			if oldest == nil || seg.Start.Before(oldest.Start) {
				oldest = seg
			}
			break // sorted, first non-recording item is the oldest
		}
	}
	return oldest
}

func sortSegments(items []*record.Segment) {
	sort.Slice(items, func(i, j int) bool {
		return items[i].Start.Before(items[j].Start)
	})
}
