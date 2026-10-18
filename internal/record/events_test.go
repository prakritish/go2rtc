package record

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/record"
	"github.com/stretchr/testify/require"
)

// fakeSegments creates segments of 10 seconds for the stream: from now-age till now
func fakeSegments(t *testing.T, stream string, now time.Time, age time.Duration) {
	for start := now.Add(-age); start.Before(now); start = start.Add(10 * time.Second) {
		path := filepath.Join(cat.streamDir(stream), filepath.FromSlash(record.SegmentID(start)))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0755))
		require.NoError(t, os.WriteFile(path, make([]byte, 10), 0644))

		idx := []byte("G2RIDX01")
		idx = append(idx, 2, 0, 0, 0, 0, 0, 0x27, 0x10, 0, 0, 0, 0, 0, 0, 0, 10) // end: 10s
		require.NoError(t, os.WriteFile(record.IndexPath(path), idx, 0644))
	}
	require.NoError(t, cat.scan())
}

func setupEvents(t *testing.T) {
	cat = newCatalog(t.TempDir())
	events = &eventStore{items: map[string][]*Event{}}
	cfg.pre = 10 * time.Second
	cfg.post = 10 * time.Second
	cfg.maxEvent = time.Hour
	t.Cleanup(func() {
		recordersMu.Lock()
		recorders = map[string]*recorder{}
		recordersMu.Unlock()
	})
}

func TestEventsCleanup(t *testing.T) {
	setupEvents(t)

	now := time.Now().Truncate(time.Second)
	fakeSegments(t, "cam1", now, 2*time.Minute) // 12 segments: -120s .. -10s
	fakeSegments(t, "cam2", now, 2*time.Minute)

	recordersMu.Lock()
	recorders["cam1"] = &recorder{name: "cam1", mode: modeEvents}
	recorders["cam2"] = &recorder{name: "cam2", mode: modeContinuous}
	recordersMu.Unlock()

	// event -65s .. -55s => keep -75s .. -45s
	end := now.Add(-55 * time.Second)
	require.NoError(t, events.add("cam1", &Event{Start: now.Add(-65 * time.Second), End: &end, Label: "motion"}))

	cleanupEvents(now)

	var starts []int
	for _, seg := range cat.list("cam1", time.Time{}, time.Time{}) {
		starts = append(starts, int(seg.Start.Sub(now).Seconds()))
	}
	// -80..-70 overlaps window start, -50..-40 overlaps window end,
	// -20 and -10 are the pre-event buffer (end after now - pre - 5s margin)
	require.Equal(t, []int{-80, -70, -60, -50, -20, -10}, starts)

	// continuous mode is not affected
	require.Len(t, cat.list("cam2", time.Time{}, time.Time{}), 12)

	// event in progress keeps everything after its start
	require.NoError(t, events.add("cam1", &Event{Start: now.Add(-15 * time.Second)}))
	fakeSegments(t, "cam1", now.Add(40*time.Second), 30*time.Second) // +10, +20, +30
	cleanupEvents(now.Add(40 * time.Second))
	starts = starts[:0]
	for _, seg := range cat.list("cam1", time.Time{}, time.Time{}) {
		starts = append(starts, int(seg.Start.Sub(now).Seconds()))
	}
	// window from -25s: -20 and later are kept
	require.Equal(t, []int{-80, -70, -60, -50, -20, -10, 10, 20, 30}, starts)
}

func TestEventsStore(t *testing.T) {
	setupEvents(t)
	now := time.Now()

	e := &Event{Start: now.Add(-time.Minute), Label: "door"}
	require.NoError(t, events.add("cam/1", e))
	require.Len(t, e.ID, 10)

	// persisted and loaded
	events = &eventStore{items: map[string][]*Event{}}
	events.load(cat.root)
	got := events.get("cam/1", e.ID)
	require.NotNil(t, got)
	require.Equal(t, "door", got.Label)
	require.Nil(t, got.End)

	// in progress: window till now + post
	from, to := got.window(now)
	require.True(t, from.Equal(e.Start.Add(-10*time.Second)))
	require.True(t, to.Equal(now.Add(10*time.Second)))

	ended, err := events.end("cam/1", e.ID, now)
	require.NoError(t, err)
	require.True(t, ended.End.Equal(now))

	_, err = events.end("cam/1", "nope", now)
	require.ErrorIs(t, err, errEventNotFound)
	_, err = events.end("cam/1", e.ID, now.Add(-time.Hour))
	require.Error(t, err)

	require.Len(t, events.list("cam/1", now.Add(-30*time.Second), time.Time{}), 1)
	require.Len(t, events.list("cam/1", now.Add(time.Second), time.Time{}), 0)

	// auto end after max duration, event without recordings is removed later
	long := &Event{Start: now.Add(-2 * time.Hour)}
	require.NoError(t, events.add("cam/1", long))
	events.maintain(now)
	require.Nil(t, events.get("cam/1", long.ID), "ended event without recording should be removed")

	require.NoError(t, events.remove("cam/1", e.ID))
	_, err = os.Stat(events.path("cam/1"))
	require.True(t, os.IsNotExist(err), "empty events file should be removed")
}

func TestEventsActive(t *testing.T) {
	setupEvents(t)
	now := time.Now()

	require.False(t, events.active("cam1", now))

	end := now.Add(-5 * time.Second) // ended 5s ago, post 10s
	require.NoError(t, events.add("cam1", &Event{Start: now.Add(-time.Minute), End: &end}))
	require.True(t, events.active("cam1", now))
	require.False(t, events.active("cam1", now.Add(6*time.Second)))

	future := &Event{Start: now.Add(time.Minute)}
	require.NoError(t, events.add("cam2", future))
	require.False(t, events.active("cam2", now), "event in the future")
	require.True(t, events.active("cam2", now.Add(2*time.Minute)), "open event")
}
