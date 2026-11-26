package record

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/record"
	"github.com/stretchr/testify/require"
)

// writeSegment creates a segment file with an index, closed - with the end entry (10 seconds)
func writeSegment(t *testing.T, c *catalog, stream string, start time.Time, data []byte, closed bool) string {
	path := filepath.Join(c.streamDir(stream), filepath.FromSlash(record.SegmentID(start)))
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0755))
	require.NoError(t, os.WriteFile(path, data, 0644))

	idx := []byte("G2RIDX01")
	if closed {
		idx = append(idx, 2, 0, 0, 0, 0, 0, 0x27, 0x10, 0, 0, 0, 0, 0, 0, 0, byte(len(data))) // end: 10s
	}
	require.NoError(t, os.WriteFile(record.IndexPath(path), idx, 0644))
	return path
}

func TestScanWhileRecording(t *testing.T) {
	c := newCatalog(t.TempDir())
	old := c.since.Add(-time.Hour).Truncate(time.Second)

	// recorded before this run
	writeSegment(t, c, "cam", old, make([]byte, 10), true)
	writeSegment(t, c, "cam", old.Add(10*time.Second), make([]byte, 10), true)
	writeSegment(t, c, "other", old, make([]byte, 10), true)

	// recorded by this run and already in the catalog: the catalog info is kept, the file isn't touched
	live := c.since.Add(time.Second).Truncate(time.Millisecond)
	livePath := writeSegment(t, c, "cam", live, []byte("not an mp4, repair would change it"), false)
	c.update(record.Segment{Stream: "cam", ID: record.SegmentID(live), Start: live, Path: livePath, Size: 1000, Recording: true})

	// being created by this run, not in the catalog yet: skipped, the file isn't touched
	newPath := writeSegment(t, c, "cam", live.Add(time.Minute), []byte("not an mp4 either"), false)

	require.False(t, c.ready())
	require.False(t, c.complete(time.Time{}))
	require.False(t, c.complete(old))
	require.True(t, c.complete(c.since))

	require.NoError(t, c.scan())
	require.True(t, c.ready())
	require.True(t, c.complete(old))

	items := c.list("cam", time.Time{}, time.Time{})
	require.Len(t, items, 3)
	require.True(t, items[0].Start.Equal(old))
	require.True(t, items[1].Start.Equal(old.Add(10*time.Second)))
	require.Equal(t, livePath, items[2].Path)
	require.True(t, items[2].Recording)
	require.Equal(t, int64(1000), items[2].Size)
	require.Len(t, c.list("other", time.Time{}, time.Time{}), 1)

	for path, data := range map[string]string{
		livePath: "not an mp4, repair would change it",
		newPath:  "not an mp4 either",
	} {
		b, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Equal(t, data, string(b), path)
	}

	// a second scan doesn't add duplicates
	require.NoError(t, c.scan())
	require.Len(t, c.list("cam", time.Time{}, time.Time{}), 3)
}

func TestScanParallel(t *testing.T) {
	for _, jobs := range []int{1, 7, 1000} {
		c := newCatalog(t.TempDir())
		c.scanJobs = jobs
		base := c.since.Add(-24 * time.Hour).Truncate(time.Second)
		for _, stream := range []string{"a", "b", "c"} {
			for i := 0; i < 40; i++ {
				writeSegment(t, c, stream, base.Add(time.Duration(i)*10*time.Second), make([]byte, 10), true)
			}
		}

		require.NoError(t, c.scan())
		require.Equal(t, map[string]int64{"a": 400, "b": 400, "c": 400}, c.streams(), "jobs=%d", jobs)
		items := c.list("b", time.Time{}, time.Time{})
		for i := 1; i < len(items); i++ {
			require.True(t, items[i-1].Start.Before(items[i].Start))
		}
	}
}

func TestAPIWhileLoading(t *testing.T) {
	oldCat := cat
	t.Cleanup(func() { cat = oldCat })

	cat = newCatalog(t.TempDir())
	old := cat.since.Add(-time.Hour).Truncate(time.Second)
	path := writeSegment(t, cat, "cam", old, make([]byte, 10), true)
	id := record.SegmentID(old)

	get := func(handler http.HandlerFunc, target string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		handler(rec, httptest.NewRequest(http.MethodGet, target, nil))
		return rec
	}
	start := old.Format(time.RFC3339)
	recent := cat.since.Add(time.Second).Format(time.RFC3339Nano)

	// older recordings are unknown: 503, not a wrong (empty) answer
	for _, tc := range []struct {
		handler http.HandlerFunc
		target  string
	}{
		{apiTimeline, "/api/record/timeline?src=cam"},
		{apiTimeline, "/api/record/timeline?src=cam&start=" + start},
		{apiSegments, "/api/record/segments?src=cam&start=" + start},
		{apiVideo, "/api/record/video.mp4?src=cam&duration=10&start=" + start},
		{apiPlaylist, "/api/record/playlist.m3u8?src=cam&duration=10&start=" + start},
		{apiSegment, "/api/record/segment.mp4?src=cam&id=" + id},
	} {
		rec := get(tc.handler, tc.target)
		require.Equal(t, http.StatusServiceUnavailable, rec.Code, tc.target)
		require.Equal(t, "5", rec.Header().Get("Retry-After"), tc.target)
	}

	_, err := playerHandler("record:cam?duration=10&start=" + start)
	require.ErrorContains(t, err, "being loaded")

	// the recording of this run is complete
	require.Equal(t, http.StatusOK, get(apiTimeline, "/api/record/timeline?src=cam&start="+recent).Code)
	require.Equal(t, http.StatusOK, get(apiSegments, "/api/record/segments?src=cam&start="+recent).Code)

	// the status says it
	rec := get(apiRecord, "/api/record")
	require.Contains(t, rec.Body.String(), `"loading": true`)

	require.NoError(t, cat.scan())

	rec = get(apiTimeline, "/api/record/timeline?src=cam&start="+start)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `"ranges":[{`)
	require.Equal(t, http.StatusOK, get(apiSegment, "/api/record/segment.mp4?src=cam&id="+id).Code)
	require.Equal(t, http.StatusNotFound, get(apiSegment, "/api/record/segment.mp4?src=cam&id=nope").Code)
	require.NotContains(t, get(apiRecord, "/api/record").Body.String(), `"loading"`)
	require.FileExists(t, path)
}

func TestEventsKeptWhileLoading(t *testing.T) {
	setupEvents(t)
	cat = newCatalog(t.TempDir()) // not scanned yet

	now := time.Now()
	end := now.Add(-time.Hour)
	events.items["cam"] = []*Event{{ID: "e1", Start: end.Add(-time.Minute), End: &end}}

	// no recording for the event in the incomplete catalog: kept, it can be still loading
	events.maintain(now)
	require.NotNil(t, events.get("cam", "e1"))

	require.NoError(t, cat.scan())
	events.maintain(now)
	require.Nil(t, events.get("cam", "e1"), "ended event without recording is removed")
}
