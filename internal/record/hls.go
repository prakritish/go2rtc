package record

import (
	"errors"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/AlexxIT/go2rtc/internal/api"
	"github.com/AlexxIT/go2rtc/pkg/record"
)

// apiPlaylist - HLS VOD playlist for the time range (for Safari and seeking in hls.js)
// GET /api/record/playlist.m3u8?src=cam1&start=2026-10-17T14:00:00Z&duration=300
// GET /api/record/playlist.m3u8?src=cam1&start=2026-10-17T14:00:00Z&end=2026-10-17T14:05:00Z
//
// Media segments are byte ranges of the video.mp4 clip for the same time range,
// so there is no additional storage or processing.
func apiPlaylist(w http.ResponseWriter, r *http.Request) {
	if !allowMethod(w, r, "GET", "HEAD") {
		return
	}

	src, from, to, err := parseRange(r.URL.Query())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	clip, err := getClip(src, from, to, false)
	if err != nil {
		if errors.Is(err, record.ErrNoRecording) {
			http.Error(w, err.Error(), http.StatusNotFound)
		} else {
			log.Error().Err(err).Caller().Send()
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}

	// exactly the same time range, so video.mp4 will use the same (cached) clip
	query := url.Values{}
	query.Set("src", src)
	query.Set("start", from.UTC().Format(time.RFC3339Nano))
	query.Set("end", to.UTC().Format(time.RFC3339Nano))
	uri := "video.mp4?" + query.Encode()

	// the range includes the segment being recorded: the playlist grows, the player reloads it
	var live bool
	if to.After(time.Now()) {
		if items := cat.list(src, from, to); len(items) > 0 && items[len(items)-1].Recording {
			live = true
		}
	}

	api.Response(w, buildPlaylist(clip, uri, live), "application/vnd.apple.mpegurl")
}

type hlsSegment struct {
	offset   int64
	size     int64
	time     time.Duration // media time in the clip
	duration time.Duration
	real     time.Time
}

// hlsSegments splits the clip by keyframes. A new segment starts when the current one
// is long enough or when there is a gap in the recording (for correct program date time).
func hlsSegments(keyframes []record.ClipKeyframe, size int64, duration, target time.Duration) []hlsSegment {
	var segments []hlsSegment

	for _, kf := range keyframes {
		if n := len(segments); n > 0 {
			last := &segments[n-1]
			media := kf.Time - last.time
			jump := kf.Real.Sub(last.real) - media // real time jumps over the gap
			if media < target && jump < time.Second && jump > -time.Second {
				continue
			}
		}
		segments = append(segments, hlsSegment{offset: kf.Offset, time: kf.Time, real: kf.Real})
	}

	// the first segment also contains preroll (audio before the first keyframe)
	if len(segments) > 0 {
		segments[0].time = 0
	}

	for i := range segments {
		end, endTime := size, duration
		if i+1 < len(segments) {
			end, endTime = segments[i+1].offset, segments[i+1].time
		}
		segments[i].size = end - segments[i].offset
		segments[i].duration = endTime - segments[i].time
	}

	return segments
}

// buildPlaylist - VOD playlist or EVENT playlist (live) that grows while the segment is recorded
func buildPlaylist(clip *record.Clip, uri string, live bool) string {
	segments := hlsSegments(clip.Keyframes, clip.Size(), clip.Duration, cfg.hlsSegment)

	if live && len(segments) > 0 {
		// the last segment is not finished (no next keyframe yet), segments can't change after listing
		segments = segments[:len(segments)-1]
	}

	var target float64
	for _, seg := range segments {
		target = math.Max(target, seg.duration.Seconds())
	}
	if live {
		// target duration can't change between reloads, keep a margin for longer keyframe intervals
		target = math.Max(target, 2*cfg.hlsSegment.Seconds())
	}

	playlistType := "VOD"
	if live {
		playlistType = "EVENT"
	}

	sb := strings.Builder{}
	sb.WriteString("#EXTM3U\n")
	sb.WriteString("#EXT-X-VERSION:7\n")
	sb.WriteString("#EXT-X-PLAYLIST-TYPE:" + playlistType + "\n")
	sb.WriteString("#EXT-X-TARGETDURATION:" + strconv.Itoa(int(math.Ceil(target))) + "\n")
	sb.WriteString("#EXT-X-MEDIA-SEQUENCE:0\n")
	sb.WriteString("#EXT-X-INDEPENDENT-SEGMENTS\n")
	sb.WriteString(`#EXT-X-MAP:URI="` + uri + `",BYTERANGE="` + strconv.FormatInt(clip.InitSize(), 10) + "@0\"\n")

	for _, seg := range segments {
		sb.WriteString("#EXT-X-PROGRAM-DATE-TIME:" + seg.real.UTC().Format("2006-01-02T15:04:05.000Z") + "\n")
		sb.WriteString("#EXTINF:" + strconv.FormatFloat(seg.duration.Seconds(), 'f', 3, 64) + ",\n")
		sb.WriteString("#EXT-X-BYTERANGE:" + strconv.FormatInt(seg.size, 10) + "@" + strconv.FormatInt(seg.offset, 10) + "\n")
		sb.WriteString(uri + "\n")
	}

	if !live {
		sb.WriteString("#EXT-X-ENDLIST\n")
	}
	return sb.String()
}
