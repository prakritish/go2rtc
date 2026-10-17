package record

import (
	"errors"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/AlexxIT/go2rtc/internal/api"
	"github.com/AlexxIT/go2rtc/internal/app"
	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/record"
)

// apiRecord
// GET    /api/record                       - status of all recorders and storage
// PUT    /api/record?src=cam1[&video&audio] - start recording (saved to config)
// DELETE /api/record?src=cam1               - stop recording (removed from config)
func apiRecord(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		recordersMu.Lock()
		status := struct {
			Path      string               `json:"path"`
			Segment   float64              `json:"segment"`
			Retention float64              `json:"retention,omitempty"`
			MaxSize   int64                `json:"max_size,omitempty"`
			TotalSize int64                `json:"total_size"`
			Sizes     map[string]int64     `json:"sizes"`
			Streams   map[string]*recorder `json:"streams"`
		}{
			Path:      cfg.path,
			Segment:   cfg.segment.Seconds(),
			Retention: cfg.retention.Seconds(),
			MaxSize:   cfg.maxSize,
			Sizes:     cat.streams(),
			Streams:   make(map[string]*recorder, len(recorders)),
		}
		for name, rec := range recorders {
			status.Streams[name] = rec
		}
		recordersMu.Unlock()

		for _, size := range status.Sizes {
			status.TotalSize += size
		}

		api.ResponsePrettyJSON(w, status)
		return
	}

	query := r.URL.Query()
	src := query.Get("src")
	if src == "" {
		http.Error(w, "src is required", http.StatusBadRequest)
		return
	}

	switch r.Method {
	case "PUT":
		// keep only media params, like api/preload
		for k := range query {
			switch k {
			case core.KindVideo, core.KindAudio, "mode":
			default:
				delete(query, k)
			}
		}

		rawQuery := query.Encode()

		if err := Start(src, rawQuery); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		if err := app.PatchConfig([]string{"record", "streams", src}, rawQuery); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}

	case "DELETE":
		if err := Stop(src); err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}

		if err := app.PatchConfig([]string{"record", "streams", src}, nil); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}

	default:
		http.Error(w, "", http.StatusMethodNotAllowed)
	}
}

// apiSegments
// GET /api/record/segments?src=cam1[&start=...][&end=...]
// start and end - RFC3339 time or unix seconds
func apiSegments(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	src := query.Get("src")
	if src == "" {
		http.Error(w, "src is required", http.StatusBadRequest)
		return
	}

	from, err := parseTime(query.Get("start"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	to, err := parseTime(query.Get("end"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	api.ResponseJSON(w, cat.list(src, from, to))
}

// apiSegment
// GET /api/record/segment.mp4?src=cam1&id=2026-10-17/2026-10-17T14-03-00.123Z.mp4
// Supports HTTP Range requests. The id is looked up in the catalog, so no path traversal.
func apiSegment(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()

	seg := cat.get(query.Get("src"), query.Get("id"))
	if seg == nil {
		http.Error(w, "segment not found", http.StatusNotFound)
		return
	}

	f, err := os.Open(seg.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	defer f.Close()

	w.Header().Set("Content-Type", "video/mp4")
	if filename := query.Get("filename"); filename != "" {
		w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	}

	http.ServeContent(w, r, "", seg.Start, f)
}

// parseTime - RFC3339 time, unix seconds, "now" or seconds relative to now ("-60")
func parseTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	if s == "now" {
		return time.Now(), nil
	}
	if strings.HasPrefix(s, "-") {
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return time.Now().Add(time.Duration(f * float64(time.Second))), nil
		}
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return time.UnixMilli(int64(f * 1000)), nil
	}
	return time.Time{}, errors.New("record: wrong time format: " + s)
}

// gapTolerance - distance between segments that is not counted as a gap
const gapTolerance = time.Second

type timeRange struct {
	Start    time.Time `json:"start"`
	End      time.Time `json:"end"`
	Duration float64   `json:"duration"`
}

func newTimeRange(start, end time.Time) timeRange {
	return timeRange{Start: start, End: end, Duration: end.Sub(start).Seconds()}
}

// apiTimeline - continuous recorded ranges and gaps between them
// GET /api/record/timeline?src=cam1[&start=...][&end=...]
func apiTimeline(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	src := query.Get("src")
	if src == "" {
		http.Error(w, "src is required", http.StatusBadRequest)
		return
	}

	from, err := parseTime(query.Get("start"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	to, err := parseTime(query.Get("end"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	ranges, gaps := timeline(cat.list(src, from, to), from, to)

	api.ResponseJSON(w, struct {
		Ranges []timeRange `json:"ranges"`
		Gaps   []timeRange `json:"gaps"`
		Events []Event     `json:"events"`
	}{ranges, gaps, events.list(src, from, to)})
}

func timeline(segments []record.Segment, from, to time.Time) (ranges, gaps []timeRange) {
	ranges = make([]timeRange, 0)
	gaps = make([]timeRange, 0)

	for _, seg := range segments {
		start, end := seg.Start, seg.End()
		if !from.IsZero() && start.Before(from) {
			start = from
		}
		if !to.IsZero() && end.After(to) {
			end = to
		}
		if !end.After(start) {
			continue
		}

		if n := len(ranges); n > 0 && start.Sub(ranges[n-1].End) <= gapTolerance {
			if end.After(ranges[n-1].End) {
				ranges[n-1] = newTimeRange(ranges[n-1].Start, end)
			}
			continue
		}

		if n := len(ranges); n > 0 {
			gaps = append(gaps, newTimeRange(ranges[n-1].End, start))
		}
		ranges = append(ranges, newTimeRange(start, end))
	}

	return
}

// apiVideo - one MP4 file for the time range, without transcoding
// GET /api/record/video.mp4?src=cam1&start=2026-10-17T14:00:00Z&duration=300
// GET /api/record/video.mp4?src=cam1&start=2026-10-17T14:00:00Z&end=2026-10-17T14:05:00Z
// Optional:
// - format=mp4 - regular MP4 (moov at start) via FFmpeg, without transcoding
// - exact      - start exactly from the requested time (MP4 edit list via FFmpeg, without transcoding)
func apiVideo(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()

	src, from, to, err := parseRange(query)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// HLS segments are byte ranges of one clip, cache it for them
	clip, err := getClip(src, from, to, r.Header.Get("Range") != "")
	if err != nil {
		if errors.Is(err, record.ErrNoRecording) {
			http.Error(w, err.Error(), http.StatusNotFound)
		} else {
			log.Error().Err(err).Caller().Send()
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}

	header := w.Header()
	header.Set("X-Record-Start", clip.Start.UTC().Format(time.RFC3339Nano))
	header.Set("X-Record-End", clip.End.UTC().Format(time.RFC3339Nano))
	header.Set("X-Record-Duration", strconv.FormatFloat(clip.Duration.Seconds(), 'f', 3, 64))
	header.Set("X-Record-Gaps", strconv.Itoa(clip.Gaps))
	if clip.Truncated {
		header.Set("X-Record-Truncated", "codec change")
	}
	if filename := query.Get("filename"); filename != "" {
		header.Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	}

	if query.Has("exact") || query.Get("format") == "mp4" {
		var skip time.Duration
		if query.Has("exact") {
			// media time of the requested start: preroll + time from the start keyframe
			skip = clip.Preroll
			if from.After(clip.Start) {
				skip += from.Sub(clip.Start)
				header.Set("X-Record-Start", from.UTC().Format(time.RFC3339Nano))
			}
			header.Set("X-Record-Duration", strconv.FormatFloat((clip.Duration-skip).Seconds(), 'f', 3, 64))
		}
		serveFFmpeg(w, r, clip, skip, src+"|"+strconv.FormatInt(from.UnixNano(), 10)+"|"+strconv.FormatInt(to.UnixNano(), 10))
		return
	}

	if clip.Codecs != "" {
		header.Set("Content-Type", `video/mp4; codecs="`+clip.Codecs+`"`)
	} else {
		header.Set("Content-Type", "video/mp4")
	}

	rd := clip.NewReader()
	defer rd.Close()

	http.ServeContent(w, r, "", time.Time{}, rd)
}

// parseRange parses src, start and end or duration params
func parseRange(query url.Values) (src string, from, to time.Time, err error) {
	if src = query.Get("src"); src == "" {
		err = errors.New("src is required")
		return
	}

	// event with pre and post time
	if id := query.Get("event"); id != "" {
		e := events.get(src, id)
		if e == nil {
			err = errEventNotFound
			return
		}
		from, to = e.window(time.Now())
		return
	}

	if from, err = parseTime(query.Get("start")); err != nil || from.IsZero() {
		err = errors.New("start is required: RFC3339 time or unix seconds")
		return
	}

	if s := query.Get("end"); s != "" {
		if to, err = parseTime(s); err != nil {
			return
		}
	} else if s = query.Get("duration"); s != "" {
		var d time.Duration
		if d, err = parseClipDuration(s); err != nil {
			return
		}
		to = from.Add(d)
	} else {
		err = errors.New("end or duration is required")
		return
	}

	if !to.After(from) {
		err = errors.New("end should be after start")
	} else if cfg.maxClip > 0 && to.Sub(from) > cfg.maxClip {
		err = errors.New("max duration is " + cfg.maxClip.String())
	}
	return
}

// clip cache: HLS player requests many byte ranges of the same clip
const clipCacheTTL = 2 * time.Minute
const clipCacheSize = 32

type cachedClip struct {
	clip    *record.Clip
	expires time.Time
}

var clipCache = map[string]*cachedClip{}
var clipCacheMu sync.Mutex

// getClip builds a new clip or returns cached one if useCache.
// Byte ranges of the clip don't change while the recording grows (new data is added
// to the end), so cached clip is consistent with the HLS playlist built earlier.
func getClip(src string, from, to time.Time, useCache bool) (*record.Clip, error) {
	key := src + "|" + strconv.FormatInt(from.UnixNano(), 10) + "|" + strconv.FormatInt(to.UnixNano(), 10)
	now := time.Now()

	clipCacheMu.Lock()
	for k, v := range clipCache {
		if now.After(v.expires) {
			delete(clipCache, k)
		}
	}
	if v := clipCache[key]; v != nil && useCache {
		v.expires = now.Add(clipCacheTTL)
		clipCacheMu.Unlock()
		return v.clip, nil
	}
	clipCacheMu.Unlock()

	clip, err := record.NewClip(cat.list(src, from, to), from, to)
	if err != nil {
		return nil, err
	}

	clipCacheMu.Lock()
	if len(clipCache) >= clipCacheSize {
		// remove the oldest
		var oldest string
		for k, v := range clipCache {
			if oldest == "" || v.expires.Before(clipCache[oldest].expires) {
				oldest = k
			}
		}
		delete(clipCache, oldest)
	}
	clipCache[key] = &cachedClip{clip: clip, expires: now.Add(clipCacheTTL)}
	clipCacheMu.Unlock()

	return clip, nil
}

// parseClipDuration - seconds ("300", "12.5") or duration ("5m", "1h30m")
func parseClipDuration(s string) (time.Duration, error) {
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		if f <= 0 {
			return 0, errors.New("record: duration should be positive")
		}
		return time.Duration(f * float64(time.Second)), nil
	}
	d, err := parseDuration(s)
	if err == nil && d <= 0 {
		err = errors.New("record: duration should be positive")
	}
	return d, err
}

// apiEvents
// GET    /api/record/events?src=cam1[&start=...][&end=...]                - list events
// POST   /api/record/events?src=cam1[&label=motion][&start=][&duration=|&end=] - create event (start=now by default),
//
//	without duration or end - event in progress
//
// PUT    /api/record/events?src=cam1&id=ID[&end=]                         - end event in progress (end=now by default)
// DELETE /api/record/events?src=cam1&id=ID                                - delete event
func apiEvents(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	src := query.Get("src")
	if src == "" {
		http.Error(w, "src is required", http.StatusBadRequest)
		return
	}

	switch r.Method {
	case "GET":
		from, err := parseTime(query.Get("start"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		to, err := parseTime(query.Get("end"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		api.ResponseJSON(w, events.list(src, from, to))

	case "POST":
		e := &Event{Label: query.Get("label"), Start: time.Now()}

		if s := query.Get("start"); s != "" {
			t, err := parseTime(s)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			e.Start = t
		}

		if query.Has("end") || query.Has("duration") {
			query.Set("start", e.Start.Format(time.RFC3339Nano))
			_, _, end, err := parseRange(query)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			e.End = &end
		}

		if err := events.add(src, e); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		log.Debug().Msgf("[record] new event id=%s stream=%s label=%s", e.ID, src, e.Label)
		notifyEvents(src)
		api.ResponseJSON(w, e)

	case "PUT":
		end := time.Now()
		if s := query.Get("end"); s != "" {
			var err error
			if end, err = parseTime(s); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
		}
		e, err := events.end(src, query.Get("id"), end)
		if err != nil {
			status := http.StatusBadRequest
			if errors.Is(err, errEventNotFound) {
				status = http.StatusNotFound
			}
			http.Error(w, err.Error(), status)
			return
		}
		notifyEvents(src)
		api.ResponseJSON(w, e)

	case "DELETE":
		if err := events.remove(src, query.Get("id")); err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		notifyEvents(src)

	default:
		http.Error(w, "", http.StatusMethodNotAllowed)
	}
}
