package record

import (
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

// Layout: {root}/{stream}/{YYYY-MM-DD}/{YYYY-MM-DD}T{hh-mm-ss.mmm}Z.mp4 (UTC)

const (
	dateLayout = "2006-01-02"
	fileLayout = "2006-01-02T15-04-05.000Z"

	ExtVideo = ".mp4"
	ExtIndex = ".idx"
)

// EscapeName converts stream name to safe directory name
func EscapeName(name string) string {
	s := url.PathEscape(name)
	if strings.HasPrefix(s, ".") {
		s = "%2E" + s[1:] // protect from "." and ".."
	}
	return s
}

func UnescapeName(dir string) (string, error) {
	return url.PathUnescape(dir)
}

// SegmentID returns segment path relative to the stream dir
func SegmentID(start time.Time) string {
	start = start.UTC()
	return start.Format(dateLayout) + "/" + start.Format(fileLayout) + ExtVideo
}

// ParseSegmentID returns segment start time from the segment ID or file name
func ParseSegmentID(id string) (time.Time, bool) {
	name := filepath.Base(id)
	if !strings.HasSuffix(name, ExtVideo) {
		return time.Time{}, false
	}
	t, err := time.ParseInLocation(fileLayout, strings.TrimSuffix(name, ExtVideo), time.UTC)
	return t, err == nil
}

func IndexPath(videoPath string) string {
	return strings.TrimSuffix(videoPath, ExtVideo) + ExtIndex
}
