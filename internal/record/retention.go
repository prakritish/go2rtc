package record

import (
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/record"
)

func runRetention(interval time.Duration) {
	for {
		cleanup()
		time.Sleep(interval)
	}
}

func runEventsCleanup(interval time.Duration) {
	for {
		time.Sleep(interval)
		now := time.Now()
		events.maintain(now)
		cleanupEvents(now)
	}
}

func cleanup() {
	if cfg.retention > 0 {
		cutoff := time.Now().Add(-cfg.retention)
		count, size := cat.remove(func(seg *record.Segment) bool {
			return seg.End().Before(cutoff)
		})
		if count > 0 {
			log.Debug().Msgf("[record] retention removed %d segments, %d bytes", count, size)
		}
	}

	if cfg.maxSize > 0 {
		for cat.totalSize() > cfg.maxSize {
			oldest := cat.oldest()
			if oldest == nil {
				break
			}
			count, size := cat.remove(func(seg *record.Segment) bool {
				return seg.Path == oldest.Path
			})
			if count == 0 {
				break // can't remove, don't loop forever
			}
			log.Debug().Msgf("[record] max_size removed %s, %d bytes", oldest.Path, size)
		}
	}
}

// parseDuration - like time.ParseDuration, but with support days, ex. "7d", "1d12h"
func parseDuration(s string) (time.Duration, error) {
	if s == "" || s == "0" {
		return 0, nil
	}

	var d time.Duration
	if days, rest, ok := strings.Cut(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil {
			return 0, err
		}
		d = time.Duration(n) * 24 * time.Hour
		if rest == "" {
			return d, nil
		}
		s = rest
	}

	v, err := time.ParseDuration(s)
	return d + v, err
}

// parseSize parses sizes like "500GB", "1.5TB", "200M". Units are powers of 1024.
func parseSize(s string) (int64, error) {
	s = strings.ToUpper(strings.TrimSpace(s))
	if s == "" || s == "0" {
		return 0, nil
	}

	s = strings.TrimSuffix(strings.TrimSuffix(s, "B"), "I") // GB, GIB => G

	mul := float64(1)
	if n := len(s); n > 0 {
		if i := strings.IndexByte("KMGT", s[n-1]); i >= 0 {
			mul = float64(int64(1) << (10 * (i + 1)))
			s = s[:n-1]
		}
	}

	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0, err
	}
	if f < 0 {
		return 0, errors.New("record: negative size")
	}
	return int64(f * mul), nil
}
