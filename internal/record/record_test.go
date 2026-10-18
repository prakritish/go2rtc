package record

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/record"
	"github.com/stretchr/testify/require"
)

func TestParseDuration(t *testing.T) {
	tests := map[string]time.Duration{
		"":      0,
		"0":     0,
		"60s":   time.Minute,
		"7d":    7 * 24 * time.Hour,
		"1d12h": 36 * time.Hour,
		"168h":  168 * time.Hour,
	}
	for s, expect := range tests {
		d, err := parseDuration(s)
		require.NoError(t, err, s)
		require.Equal(t, expect, d, s)
	}

	_, err := parseDuration("xd")
	require.Error(t, err)
}

func TestParseSize(t *testing.T) {
	tests := map[string]int64{
		"":       0,
		"1024":   1024,
		"10K":    10 << 10,
		"200MB":  200 << 20,
		"500GB":  500 << 30,
		"1.5TB":  3 << 39,
		"2 GiB":  2 << 30,
		"100 mb": 100 << 20,
	}
	for s, expect := range tests {
		n, err := parseSize(s)
		require.NoError(t, err, s)
		require.Equal(t, expect, n, s)
	}

	_, err := parseSize("abc")
	require.Error(t, err)
}

func TestParseMedias(t *testing.T) {
	medias, err := parseMedias("")
	require.NoError(t, err)
	require.Len(t, medias, 2)

	medias, err = parseMedias("video")
	require.NoError(t, err)
	require.Len(t, medias, 1)
	require.Equal(t, "video", medias[0].Kind)
	require.Len(t, medias[0].Codecs, 2)

	medias, err = parseMedias("video=h264&audio")
	require.NoError(t, err)
	require.Len(t, medias, 2)
	require.Equal(t, "H264", medias[0].Codecs[0].Name)

	_, err = parseMedias("foo")
	require.Error(t, err)
}

func TestCatalog(t *testing.T) {
	root := t.TempDir()
	cat := newCatalog(root)

	base := time.Date(2026, 10, 17, 23, 59, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		start := base.Add(time.Duration(i) * time.Minute)
		id := record.SegmentID(start)
		path := filepath.Join(cat.streamDir("cam/1"), filepath.FromSlash(id))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0755))
		require.NoError(t, os.WriteFile(path, make([]byte, 100), 0644))

		idx := []byte("G2RIDX01")
		idx = append(idx, 2, 0, 0, 0, 0, 0, 0xEA, 0x60, 0, 0, 0, 0, 0, 0, 0, 100) // end: 60s, size 100
		require.NoError(t, os.WriteFile(record.IndexPath(path), idx, 0644))
	}

	require.NoError(t, cat.scan())

	all := cat.list("cam/1", time.Time{}, time.Time{})
	require.Len(t, all, 3)
	require.Equal(t, time.Minute, all[0].Duration)
	require.True(t, all[1].Start.Equal(base.Add(time.Minute)))
	require.Equal(t, int64(300), cat.totalSize())

	// overlap query
	items := cat.list("cam/1", base.Add(90*time.Second), base.Add(100*time.Second))
	require.Len(t, items, 1)
	require.Equal(t, all[1].ID, items[0].ID)

	require.NotNil(t, cat.get("cam/1", all[2].ID))
	require.Nil(t, cat.get("cam/1", "../../etc/passwd"))

	// remove oldest crosses the date dir boundary
	count, _ := cat.remove(func(seg *record.Segment) bool { return seg.ID == all[0].ID })
	require.Equal(t, 1, count)
	_, err := os.Stat(filepath.Dir(all[0].Path))
	require.True(t, os.IsNotExist(err), "empty date dir should be removed")
	require.Len(t, cat.list("cam/1", time.Time{}, time.Time{}), 2)
}

func TestTimeline(t *testing.T) {
	base := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	seg := func(start, duration int) record.Segment {
		return record.Segment{
			Start:    base.Add(time.Duration(start) * time.Second),
			Duration: time.Duration(duration) * time.Second,
		}
	}

	// 0-60, 60.5-120 (overlap tolerance), gap, 300-360
	segments := []record.Segment{seg(0, 60), seg(60, 60), seg(300, 60)}
	segments[1].Start = segments[1].Start.Add(500 * time.Millisecond)

	ranges, gaps := timeline(segments, time.Time{}, time.Time{})
	require.Len(t, ranges, 2)
	require.Len(t, gaps, 1)
	require.True(t, ranges[0].Start.Equal(base))
	require.Equal(t, 120.5, ranges[0].Duration)
	require.True(t, gaps[0].Start.Equal(ranges[0].End))
	require.True(t, gaps[0].End.Equal(base.Add(300*time.Second)))

	// clipped by the requested range
	ranges, gaps = timeline(segments, base.Add(30*time.Second), base.Add(310*time.Second))
	require.Len(t, ranges, 2)
	require.Equal(t, 90.5, ranges[0].Duration)
	require.Equal(t, 10.0, ranges[1].Duration)
	require.Len(t, gaps, 1)
}

func TestHLSSegments(t *testing.T) {
	real := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)

	// keyframe every 2 seconds, 1000 bytes per second, gap 1 hour before keyframe at 10s
	var kfs []record.ClipKeyframe
	for i := 0; i < 8; i++ {
		ts := time.Duration(i*2) * time.Second
		r := real.Add(ts)
		if i >= 5 {
			r = r.Add(time.Hour)
		}
		kfs = append(kfs, record.ClipKeyframe{Offset: 100 + int64(i*2000), Time: ts, Real: r})
	}

	segments := hlsSegments(kfs, 100+16000, 16*time.Second, 4*time.Second)

	// 0-4, 4-8, 8-10 (cut by gap), 10-14, 14-16
	require.Len(t, segments, 5)
	expect := []struct {
		offset   int64
		size     int64
		duration time.Duration
	}{
		{100, 4000, 4 * time.Second},
		{4100, 4000, 4 * time.Second},
		{8100, 2000, 2 * time.Second},
		{10100, 4000, 4 * time.Second},
		{14100, 2000, 2 * time.Second},
	}
	for i, e := range expect {
		require.Equal(t, e.offset, segments[i].offset, "segment %d", i)
		require.Equal(t, e.size, segments[i].size, "segment %d", i)
		require.Equal(t, e.duration, segments[i].duration, "segment %d", i)
	}
	require.True(t, segments[3].real.Equal(real.Add(time.Hour+10*time.Second)))
}

func TestParseRange(t *testing.T) {
	cfg.maxClip = 24 * time.Hour
	q := func(s string) url.Values {
		v, _ := url.ParseQuery(s)
		return v
	}

	src, from, to, err := parseRange(q("src=cam1&start=2026-10-17T12:00:00Z&duration=5m"))
	require.NoError(t, err)
	require.Equal(t, "cam1", src)
	require.Equal(t, 5*time.Minute, to.Sub(from))

	_, from, to, err = parseRange(q("src=cam1&start=1792238400&end=1792238430.5"))
	require.NoError(t, err)
	require.Equal(t, 30500*time.Millisecond, to.Sub(from))

	_, from, _, err = parseRange(q("src=cam1&start=2026-10-17T17:30:00%2B05:30&duration=1"))
	require.NoError(t, err)
	require.True(t, from.Equal(time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)))

	for _, s := range []string{
		"start=2026-10-17T12:00:00Z&duration=5",           // no src
		"src=cam1&duration=5",                             // no start
		"src=cam1&start=2026-10-17T12:00:00Z",             // no end
		"src=cam1&start=2026-10-17T12:00:00Z&duration=-5", // negative
		"src=cam1&start=2026-10-17T12:00:00Z&end=2026-10-17T11:00:00Z",
		"src=cam1&start=2026-10-17T12:00:00Z&duration=25h",
	} {
		_, _, _, err = parseRange(q(s))
		require.Error(t, err, s)
	}
}

func TestSetAttachmentAndMethods(t *testing.T) {
	w := httptest.NewRecorder()
	setAttachment(w, `clip"evil.mp4`)
	require.Equal(t, `attachment; filename="clip\"evil.mp4"`, w.Header().Get("Content-Disposition"))

	for _, fn := range []http.HandlerFunc{apiSegments, apiSegment, apiTimeline, apiVideo, apiPlaylist} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/record/video.mp4?src=cam1", nil)
		fn(rec, req)
		require.Equal(t, http.StatusMethodNotAllowed, rec.Code)

		// CORS preflight
		rec = httptest.NewRecorder()
		req = httptest.NewRequest(http.MethodOptions, "/api/record/video.mp4?src=cam1", nil)
		fn(rec, req)
		require.Equal(t, http.StatusOK, rec.Code)
	}
}
