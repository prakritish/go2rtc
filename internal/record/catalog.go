package record

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/record"
)

// catalog - in-memory list of all segments, sorted by start time for each stream.
// It is rebuilt from the filesystem on startup, so the filesystem is the source of truth.
// The scan runs in the background: the recording starts right away and its new segments are
// added while the old ones are still being loaded.
type catalog struct {
	root     string
	segments map[string][]*record.Segment
	mu       sync.RWMutex
	jobs     int // parallel file removals
	scanJobs int // parallel segment loads on scan

	since    time.Time     // catalog creation: segments of this run start after it
	scanned  chan struct{} // closed when the scan is done
	scanOnce sync.Once
}

const defaultDeleteJobs = 16
const defaultScanJobs = 16

func newCatalog(root string) *catalog {
	return &catalog{
		root: root, segments: map[string][]*record.Segment{},
		jobs: defaultDeleteJobs, scanJobs: defaultScanJobs,
		since: time.Now(), scanned: make(chan struct{}),
	}
}

// ready - the scan is done, the catalog has all the segments
func (c *catalog) ready() bool {
	select {
	case <-c.scanned:
		return true
	default:
		return false
	}
}

// complete - the catalog has all the segments from the time t (zero - from the beginning)
func (c *catalog) complete(t time.Time) bool {
	return c.ready() || !t.IsZero() && !t.Before(c.since)
}

func (c *catalog) streamDir(stream string) string {
	return filepath.Join(c.root, record.EscapeName(stream))
}

// scan loads all segments from the filesystem and adds them to the catalog.
// It's safe while recording: the segments already in the catalog are kept as they are.
func (c *catalog) scan() error {
	defer c.scanOnce.Do(func() { close(c.scanned) })

	dirs, err := os.ReadDir(c.root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	type file struct{ stream, streamDir, path string }
	var files []file

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
			files = append(files, file{stream, streamDir, path})
		}
	}

	// segments are loaded by several workers: on network storage every file waits for the server
	loaded := make([]*record.Segment, len(files))
	var next atomic.Int64
	var wg sync.WaitGroup
	for range max(1, min(c.scanJobs, len(files))) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1) - 1)
				if i >= len(files) {
					return
				}
				loaded[i] = loadSegment(files[i].stream, files[i].streamDir, files[i].path, c.since)
			}
		}()
	}
	wg.Wait()

	c.mu.Lock()
	defer c.mu.Unlock()

	known := map[string]bool{}
	for _, items := range c.segments {
		for _, seg := range items {
			known[seg.Path] = true
		}
	}

	changed := map[string]bool{}
	for _, seg := range loaded {
		// segments of this run are already in the catalog and are more up to date
		if seg == nil || known[seg.Path] {
			continue
		}
		c.segments[seg.Stream] = append(c.segments[seg.Stream], seg)
		changed[seg.Stream] = true
	}
	for stream := range changed {
		sortSegments(c.segments[stream])
	}

	return nil
}

// loadSegment reads the segment info from the file and its index. The segments of this run
// (starting after since) that are not finished are skipped: they are being recorded right now.
func loadSegment(stream, streamDir, path string, since time.Time) *record.Segment {
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

	if !closed && !start.Before(since) {
		return nil
	}

	// index without the end entry or without index (crash): rebuild it from the file,
	// it's safe, because it was recorded before this run
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
			// called on every keyframe, and the start changes only on close
			// (start correction), so sort only when the order is broken
			if outOfOrder(items, i) {
				sortSegments(items)
			}
			return
		}
	}

	items = append(items, &info)
	c.segments[info.Stream] = items
	if outOfOrder(items, len(items)-1) {
		sortSegments(items)
	}
}

// outOfOrder - item i starts before its predecessor or after its successor
func outOfOrder(items []*record.Segment, i int) bool {
	return i > 0 && items[i].Start.Before(items[i-1].Start) ||
		i+1 < len(items) && items[i+1].Start.Before(items[i].Start)
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
		// the filter shares the array: drop the removed segments left in its tail
		clear(items[len(keep):])
		if len(keep) == 0 {
			delete(c.segments, stream)
		} else {
			c.segments[stream] = keep
		}
	}
	c.mu.Unlock()

	// files are removed by several workers: on network storage (NFS, SMB) every remove waits for the server
	ok := make([]bool, len(removed))
	var next atomic.Int64
	var wg sync.WaitGroup
	for range max(1, min(c.jobs, len(removed))) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1) - 1)
				if i >= len(removed) {
					return
				}
				ok[i] = removeSegmentFiles(removed[i].Path)
			}
		}()
	}
	wg.Wait()

	dirs := map[string]bool{}
	for i, seg := range removed {
		if ok[i] {
			count++
			size += seg.Size
			dirs[filepath.Dir(seg.Path)] = true
		}
	}

	// date dirs are removed once, after their segments, and only if empty
	for dir := range dirs {
		_ = os.Remove(dir)
	}

	return
}

// removeSegmentFiles removes the segment video and its index, false if the video can't be removed
func removeSegmentFiles(path string) bool {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		log.Warn().Err(err).Str("path", path).Msg("[record] can't remove segment")
		return false
	}
	_ = os.Remove(record.IndexPath(path))
	return true
}

// oldestFor returns the paths of the oldest segments of all streams (not recording)
// whose sizes add up to at least size, oldest first
func (c *catalog) oldestFor(size int64) map[string]bool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	var items []*record.Segment
	for _, stream := range c.segments {
		for _, seg := range stream {
			if !seg.Recording {
				items = append(items, seg)
			}
		}
	}
	sortSegments(items)

	paths := map[string]bool{}
	for _, seg := range items {
		if size <= 0 {
			break
		}
		paths[seg.Path] = true
		size -= seg.Size
	}
	return paths
}

func sortSegments(items []*record.Segment) {
	sort.Slice(items, func(i, j int) bool {
		return items[i].Start.Before(items[j].Start)
	})
}
