package record

import (
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/record"
)

// playerHandler - go2rtc source for playback of the recording in real time:
//
//	record:camera1?start=2026-10-17T14:00:00Z&duration=300  - time range
//	record:camera1?start=2026-10-17T14:00:00Z&end=...        - time range
//	record:camera1?start=-60                                  - follow the recording 60 seconds behind live
//	record:camera1?event=ID                                   - event with pre and post time
//
// Without end or duration, the playback follows the recording and never ends.
func playerHandler(source string) (core.Producer, error) {
	name, rawQuery, _ := strings.Cut(strings.TrimPrefix(source, "record:"), "?")
	if name == "" {
		return nil, errors.New("record: stream name is required")
	}

	query, err := url.ParseQuery(rawQuery)
	if err != nil {
		return nil, err
	}
	query.Set("src", name)

	var from, to time.Time
	if query.Has("end") || query.Has("duration") || query.Has("event") {
		if _, from, to, err = parseRange(query); err != nil {
			return nil, err
		}
	} else if from, err = parseTime(query.Get("start")); err != nil || from.IsZero() {
		return nil, errors.New("record: start is required")
	}

	return record.NewPlayer(record.PlayerOptions{
		From: from,
		To:   to,
		List: func(from, to time.Time) []record.Segment {
			return cat.list(name, from, to)
		},
	})
}
