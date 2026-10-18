package record

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/record"
)

// Event - time range of interest (motion, alarm, etc.). In the events mode only segments that
// overlap events (with pre and post time) are kept. In the continuous mode events are markers.
type Event struct {
	ID    string     `json:"id"`
	Start time.Time  `json:"start"`
	End   *time.Time `json:"end,omitempty"` // nil - event in progress
	Label string     `json:"label,omitempty"`
}

// window returns the time range of the recording that should be kept for the event
func (e *Event) window(now time.Time) (time.Time, time.Time) {
	end := now
	if e.End != nil {
		end = *e.End
	}
	return e.Start.Add(-cfg.pre), end.Add(cfg.post)
}

const eventsFile = "events.json"

type eventStore struct {
	items map[string][]*Event // sorted by start for each stream
	mu    sync.Mutex
}

var events = &eventStore{items: map[string][]*Event{}}

func (s *eventStore) path(stream string) string {
	return filepath.Join(cat.streamDir(stream), eventsFile)
}

// load reads events of all streams from the recordings dir
func (s *eventStore) load(root string) {
	dirs, _ := os.ReadDir(root)

	s.mu.Lock()
	defer s.mu.Unlock()

	for _, dir := range dirs {
		if !dir.IsDir() || dir.Name()[0] == '.' {
			continue
		}
		stream, err := record.UnescapeName(dir.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join(root, dir.Name(), eventsFile))
		if err != nil {
			continue
		}
		var items []*Event
		if err = json.Unmarshal(b, &items); err != nil {
			log.Warn().Err(err).Str("stream", stream).Msg("[record] can't load events")
			continue
		}
		sortEvents(items)
		s.items[stream] = items
	}
}

// save should be called under lock
func (s *eventStore) save(stream string) error {
	items := s.items[stream]
	path := s.path(stream)

	if len(items) == 0 {
		err := os.Remove(path)
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	b, err := json.MarshalIndent(items, "", "  ")
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	// atomic write
	tmp := path + ".tmp"
	if err = os.WriteFile(tmp, b, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (s *eventStore) add(stream string, e *Event) error {
	e.ID = core.RandString(10, 62)

	s.mu.Lock()
	defer s.mu.Unlock()

	s.items[stream] = append(s.items[stream], e)
	sortEvents(s.items[stream])
	return s.save(stream)
}

var errEventNotFound = errors.New("record: event not found")

// end sets the end time of the event in progress
func (s *eventStore) end(stream, id string, end time.Time) (*Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, e := range s.items[stream] {
		if e.ID == id {
			if end.Before(e.Start) {
				return nil, errors.New("record: end before start")
			}
			e.End = &end
			copied := *e
			return &copied, s.save(stream)
		}
	}
	return nil, errEventNotFound
}

func (s *eventStore) remove(stream, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	items := s.items[stream]
	for i, e := range items {
		if e.ID == id {
			s.items[stream] = append(items[:i], items[i+1:]...)
			return s.save(stream)
		}
	}
	return errEventNotFound
}

func (s *eventStore) get(stream, id string) *Event {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, e := range s.items[stream] {
		if e.ID == id {
			copied := *e
			return &copied
		}
	}
	return nil
}

// list returns copies of events that overlap [from, to), zero time - no limit
func (s *eventStore) list(stream string, from, to time.Time) []Event {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	result := make([]Event, 0)
	for _, e := range s.items[stream] {
		end := now
		if e.End != nil {
			end = *e.End
		}
		if !to.IsZero() && !e.Start.Before(to) {
			break
		}
		if !from.IsZero() && end.Before(from) {
			continue
		}
		result = append(result, *e)
	}
	return result
}

// active - any event is in progress (from the start till the end + post time)
func (s *eventStore) active(stream string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, e := range s.items[stream] {
		if now.Before(e.Start) {
			continue
		}
		if e.End == nil || !now.After(e.End.Add(cfg.post)) {
			return true
		}
	}
	return false
}

// notifyEvents updates the recorder of the stream immediately after events change
func notifyEvents(stream string) {
	recordersMu.Lock()
	r := recorders[stream]
	recordersMu.Unlock()
	if r != nil {
		r.updateHold()
	}
}

// windows returns time ranges to keep for the stream (events with pre and post time)
func (s *eventStore) windows(stream string, now time.Time) [][2]time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()

	var result [][2]time.Time
	for _, e := range s.items[stream] {
		from, to := e.window(now)
		result = append(result, [2]time.Time{from, to})
	}
	return result
}

// maintain ends old events in progress and removes events without recordings
func (s *eventStore) maintain(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for stream, items := range s.items {
		changed := false

		keep := items[:0]
		for _, e := range items {
			if e.End == nil && cfg.maxEvent > 0 && now.Sub(e.Start) > cfg.maxEvent {
				end := e.Start.Add(cfg.maxEvent)
				e.End = &end
				changed = true
				log.Info().Msgf("[record] auto end event id=%s stream=%s", e.ID, stream)
			}

			// remove ended events that don't have any recording anymore (retention)
			if e.End != nil {
				from, to := e.window(now)
				if len(cat.list(stream, from, to)) == 0 && now.Sub(to) > time.Minute {
					changed = true
					continue
				}
			}
			keep = append(keep, e)
		}

		if changed {
			s.items[stream] = keep
			if err := s.save(stream); err != nil {
				log.Warn().Err(err).Msg("[record] can't save events")
			}
		}
	}
}

func sortEvents(items []*Event) {
	sort.Slice(items, func(i, j int) bool {
		return items[i].Start.Before(items[j].Start)
	})
}

// cleanupEvents removes segments of streams in the events mode that don't overlap any event window.
// Segments younger than pre time are kept, they are the pre-event buffer.
func cleanupEvents(now time.Time) {
	recordersMu.Lock()
	var names []string
	for name, r := range recorders {
		if r.mode == modeEvents {
			names = append(names, name)
		}
	}
	recordersMu.Unlock()

	for _, name := range names {
		windows := events.windows(name, now)
		cutoff := now.Add(-cfg.pre - 5*time.Second)

		count, size := cat.remove(func(seg *record.Segment) bool {
			if seg.Stream != name || !seg.End().Before(cutoff) {
				return false
			}
			for _, w := range windows {
				if seg.Start.Before(w[1]) && seg.End().After(w[0]) {
					return false
				}
			}
			return true
		})
		if count > 0 {
			log.Trace().Msgf("[record] events mode removed %d segments, %d bytes, stream=%s", count, size, name)
		}
	}
}
