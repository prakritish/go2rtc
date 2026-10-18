package record

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/record"
)

// ffmpegJobs - max simultaneous FFmpeg processes for video.mp4 (created on Init)
var ffmpegJobs chan struct{}

// ffmpegTimeout - max time of one FFmpeg job
const ffmpegTimeout = 10 * time.Minute

func tmpDir() string {
	return filepath.Join(cfg.path, ".tmp")
}

// remux cache: the result is reused for HTTP Range requests and repeated downloads
const remuxCacheTTL = 5 * time.Minute
const remuxCacheSize = 8

type remuxResult struct {
	path    string
	err     error
	ready   chan struct{}
	expires time.Time
}

var remuxCache = map[string]*remuxResult{}
var remuxCacheMu sync.Mutex

// serveFFmpeg remuxes the clip to a regular MP4 (moov atom at the start) without transcoding.
// If skip > 0, FFmpeg keeps the frames from the keyframe and adds an edit list,
// so players start exactly from the requested time.
func serveFFmpeg(w http.ResponseWriter, r *http.Request, clip *record.Clip, skip time.Duration, key string) {
	// the clip size is a part of the key, because the clip with the segment being recorded grows
	key += "|" + strconv.FormatInt(clip.Size(), 10) + "|" + skip.String()

	res := getRemux(key, clip, skip)

	select {
	case <-res.ready:
	case <-r.Context().Done():
		return
	}

	if res.err != nil {
		http.Error(w, res.err.Error(), http.StatusInternalServerError)
		return
	}

	f, err := os.Open(res.path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer f.Close()

	w.Header().Set("Content-Type", "video/mp4")
	http.ServeContent(w, r, "", time.Time{}, f)
}

// getRemux returns cached or new result, only one FFmpeg job runs for the same key
func getRemux(key string, clip *record.Clip, skip time.Duration) *remuxResult {
	now := time.Now()

	remuxCacheMu.Lock()
	defer remuxCacheMu.Unlock()

	for k, v := range remuxCache {
		if now.After(v.expires) {
			removeRemux(k, v)
		}
	}

	if v := remuxCache[key]; v != nil {
		v.expires = now.Add(remuxCacheTTL)
		return v
	}

	if len(remuxCache) >= remuxCacheSize {
		var oldest string
		for k, v := range remuxCache {
			if oldest == "" || v.expires.Before(remuxCache[oldest].expires) {
				oldest = k
			}
		}
		removeRemux(oldest, remuxCache[oldest])
	}

	res := &remuxResult{ready: make(chan struct{}), expires: now.Add(remuxCacheTTL)}
	remuxCache[key] = res

	go func() {
		res.path, res.err = remux(clip, skip)
		close(res.ready)

		if res.err != nil {
			remuxCacheMu.Lock()
			if remuxCache[key] == res {
				delete(remuxCache, key)
			}
			remuxCacheMu.Unlock()
		}
	}()

	return res
}

// removeRemux should be called under lock, the file is removed after the job is done
func removeRemux(key string, res *remuxResult) {
	delete(remuxCache, key)
	go func() {
		<-res.ready
		// give the time to finish the current responses (open files work on Linux anyway)
		time.Sleep(time.Minute)
		if res.path != "" {
			_ = os.Remove(res.path)
		}
	}()
}

// remux runs FFmpeg and returns the output file path
func remux(clip *record.Clip, skip time.Duration) (string, error) {
	ffmpegJobs <- struct{}{}
	defer func() { <-ffmpegJobs }()

	if err := os.MkdirAll(tmpDir(), 0755); err != nil {
		return "", err
	}

	// faststart requires seekable output, so FFmpeg writes to a temporary file
	tmp, err := os.CreateTemp(tmpDir(), "clip-*.mp4")
	if err != nil {
		return "", err
	}
	name := tmp.Name()
	_ = tmp.Close()

	rd := clip.NewReader()
	defer rd.Close()

	input := "pipe:0"
	args := []string{"-hide_banner", "-v", "error"}

	if skip > 0 {
		// FFmpeg can't reliably seek in fMP4 from a pipe (it reads ahead and then tries to go back),
		// so the clip is written to a temporary file first
		if input, err = writeTemp(rd); err != nil {
			_ = os.Remove(name)
			return "", err
		}
		defer os.Remove(input)

		args = append(args, "-ss", strconv.FormatFloat(skip.Seconds(), 'f', 3, 64))
	}

	args = append(args,
		"-f", "mp4", "-i", input, "-map", "0", "-c", "copy",
		"-movflags", "+faststart", "-f", "mp4", "-y", name,
	)

	// not the request context: other requests can wait for the same result
	ctx, cancel := context.WithTimeout(context.Background(), ffmpegTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, cfg.ffmpeg, args...)
	if input == "pipe:0" {
		cmd.Stdin = rd
	}
	stderr := &bytes.Buffer{}
	cmd.Stderr = stderr

	log.Trace().Msgf("[record] run %s %s", cfg.ffmpeg, strings.Join(args, " "))

	if err = cmd.Run(); err != nil {
		_ = os.Remove(name)
		msg := strings.TrimSpace(stderr.String())
		log.Error().Err(err).Str("stderr", msg).Msg("[record] ffmpeg")
		return "", errors.New("ffmpeg: " + err.Error() + " " + msg)
	}

	return name, nil
}

func writeTemp(r io.Reader) (string, error) {
	f, err := os.CreateTemp(tmpDir(), "input-*.mp4")
	if err != nil {
		return "", err
	}
	if _, err = io.Copy(f, r); err == nil {
		err = f.Close()
	} else {
		_ = f.Close()
	}
	if err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}
