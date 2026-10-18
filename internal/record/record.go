package record

import (
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/AlexxIT/go2rtc/internal/api"
	"github.com/AlexxIT/go2rtc/internal/app"
	"github.com/AlexxIT/go2rtc/internal/streams"
	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/rs/zerolog"
)

func Init() {
	var conf struct {
		Mod struct {
			Path      string `yaml:"path"`
			Segment   string `yaml:"segment"`
			Retention string `yaml:"retention"`
			Pre       string `yaml:"pre"`
			Post      string `yaml:"post"`

			StallTimeout string            `yaml:"stall_timeout"`
			MaxClip      string            `yaml:"max_clip"`
			MaxEvent     string            `yaml:"max_event"`
			HLSSegment   string            `yaml:"hls_segment"`
			FFmpegJobs   int               `yaml:"ffmpeg_jobs"`
			MaxSize      string            `yaml:"max_size"`
			Streams      map[string]string `yaml:"streams"`
		} `yaml:"record"`
		FFmpeg struct {
			Bin string `yaml:"bin"`
		} `yaml:"ffmpeg"`
	}

	conf.Mod.Path = "recordings"
	conf.Mod.Segment = "60s"
	conf.Mod.Pre = "10s"
	conf.Mod.Post = "10s"
	conf.Mod.StallTimeout = "30s"
	conf.Mod.MaxClip = "24h"
	conf.Mod.MaxEvent = "1h"
	conf.Mod.HLSSegment = "4s"
	conf.Mod.FFmpegJobs = 2
	conf.FFmpeg.Bin = "ffmpeg"

	app.LoadConfig(&conf)

	log = app.GetLogger("record")

	var err error
	if cfg.path, err = filepath.Abs(conf.Mod.Path); err != nil {
		log.Error().Err(err).Caller().Send()
		return
	}
	if cfg.segment, err = parseDuration(conf.Mod.Segment); err != nil || cfg.segment <= 0 {
		log.Warn().Err(err).Msgf("[record] wrong segment: %s", conf.Mod.Segment)
		cfg.segment = time.Minute
	}
	if cfg.retention, err = parseDuration(conf.Mod.Retention); err != nil {
		log.Warn().Err(err).Msgf("[record] wrong retention: %s", conf.Mod.Retention)
	}
	if cfg.pre, err = parseDuration(conf.Mod.Pre); err != nil {
		log.Warn().Err(err).Msgf("[record] wrong pre: %s", conf.Mod.Pre)
	}
	if cfg.post, err = parseDuration(conf.Mod.Post); err != nil {
		log.Warn().Err(err).Msgf("[record] wrong post: %s", conf.Mod.Post)
	}
	cfg.stallTimeout = durationOption("stall_timeout", conf.Mod.StallTimeout, 30*time.Second)
	cfg.maxClip = durationOption("max_clip", conf.Mod.MaxClip, 24*time.Hour)
	cfg.maxEvent = durationOption("max_event", conf.Mod.MaxEvent, time.Hour)
	cfg.hlsSegment = durationOption("hls_segment", conf.Mod.HLSSegment, 4*time.Second)
	if conf.Mod.FFmpegJobs < 1 {
		conf.Mod.FFmpegJobs = 1
	}
	ffmpegJobs = make(chan struct{}, conf.Mod.FFmpegJobs)

	if cfg.maxSize, err = parseSize(conf.Mod.MaxSize); err != nil {
		log.Warn().Err(err).Msgf("[record] wrong max_size: %s", conf.Mod.MaxSize)
	}

	cfg.ffmpeg = conf.FFmpeg.Bin

	// temporary files from previous run (FFmpeg remux)
	_ = os.RemoveAll(tmpDir())

	cat = newCatalog(cfg.path)
	if err = cat.scan(); err != nil {
		log.Error().Err(err).Caller().Send()
	}
	events.load(cfg.path)

	streams.HandleFunc("record", playerHandler)

	api.HandleFunc("api/record", apiRecord)
	api.HandleFunc("api/record/segments", apiSegments)
	api.HandleFunc("api/record/segment.mp4", apiSegment)
	api.HandleFunc("api/record/timeline", apiTimeline)
	api.HandleFunc("api/record/video.mp4", apiVideo)
	api.HandleFunc("api/record/playlist.m3u8", apiPlaylist)

	api.HandleFunc("api/record/events", apiEvents)

	go runRetention(time.Minute)
	go runEventsCleanup(10 * time.Second)

	if len(conf.Mod.Streams) == 0 {
		return
	}

	// wait for other modules (like preload in streams)
	time.AfterFunc(time.Second, func() {
		for name, rawQuery := range conf.Mod.Streams {
			if err := Start(name, rawQuery); err != nil {
				log.Error().Err(err).Caller().Send()
			}
		}
	})
}

var log zerolog.Logger

var cfg struct {
	path      string
	segment   time.Duration
	retention time.Duration
	maxSize   int64
	pre       time.Duration // events mode: keep the recording before the event
	post      time.Duration // events mode: keep the recording after the event

	stallTimeout time.Duration // re-attach to the stream if no data received for this time
	maxClip      time.Duration // max duration of video.mp4 and playlist (0 - unlimited)
	maxEvent     time.Duration // event in progress is ended automatically after this time (0 - never)
	hlsSegment   time.Duration // min HLS segment duration (keyframe intervals are merged)
	ffmpeg       string
}

var cat *catalog

var recorders = map[string]*recorder{}
var recordersMu sync.Mutex

// Start recording of the stream. Recording will survive camera offline and go2rtc restarts.
func Start(name, rawQuery string) error {
	if streams.Get(name) == nil {
		return errors.New("record: stream not found: " + name)
	}

	medias, err := parseMedias(rawQuery)
	if err != nil {
		return err
	}

	mode := modeContinuous
	if query, _ := url.ParseQuery(rawQuery); query != nil {
		switch m := query.Get("mode"); m {
		case "", modeContinuous:
		case modeEvents:
			mode = modeEvents
		default:
			return errors.New("record: wrong mode: " + m)
		}
	}

	recordersMu.Lock()
	defer recordersMu.Unlock()

	if r := recorders[name]; r != nil {
		r.stop()
	}

	r := &recorder{
		name:   name,
		query:  rawQuery,
		mode:   mode,
		medias: medias,
		done:   make(chan struct{}),
		exited: make(chan struct{}),
	}
	recorders[name] = r
	go r.run()

	return nil
}

func Stop(name string) error {
	recordersMu.Lock()
	defer recordersMu.Unlock()

	r := recorders[name]
	if r == nil {
		return errors.New("record: recorder not found: " + name)
	}

	r.stop()
	delete(recorders, name)
	return nil
}

const (
	modeContinuous = "continuous"
	modeEvents     = "events" // keep only segments that overlap events
)

// parseMedias - "video&audio" (default) records all supported codecs,
// "video" or "audio" selects kinds, "video=h264" selects codecs.
func parseMedias(rawQuery string) ([]*core.Media, error) {
	if rawQuery == "" {
		rawQuery = "video&audio"
	}

	query, err := url.ParseQuery(rawQuery)
	if err != nil {
		return nil, err
	}

	var medias []*core.Media

	for _, kind := range []string{core.KindVideo, core.KindAudio} {
		values, ok := query[kind]
		if !ok {
			continue
		}

		if len(values) == 1 && values[0] == "" {
			medias = append(medias, defaultMedia(kind))
		} else {
			medias = append(medias, core.ParseQuery(map[string][]string{kind: values})...)
		}
	}

	if medias == nil {
		return nil, errors.New("record: no video or audio in query: " + rawQuery)
	}

	return medias, nil
}

// defaultMedia - all codecs supported by MP4 muxer
func defaultMedia(kind string) *core.Media {
	media := &core.Media{Kind: kind, Direction: core.DirectionSendonly}
	if kind == core.KindVideo {
		media.Codecs = []*core.Codec{
			{Name: core.CodecH264},
			{Name: core.CodecH265},
		}
	} else {
		media.Codecs = []*core.Codec{
			{Name: core.CodecAAC},
			{Name: core.CodecOpus},
			{Name: core.CodecPCMA},
			{Name: core.CodecPCMU},
			{Name: core.CodecPCM},
			{Name: core.CodecPCML},
			{Name: core.CodecMP3},
		}
	}
	return media
}

// durationOption parses the option, logs and returns the default value on error
func durationOption(name, value string, def time.Duration) time.Duration {
	d, err := parseDuration(value)
	if err != nil || d < 0 {
		log.Warn().Err(err).Msgf("[record] wrong %s: %s", name, value)
		return def
	}
	return d
}

// Shutdown properly closes all open segments, should be called before exit
func Shutdown() {
	recordersMu.Lock()
	list := make([]*recorder, 0, len(recorders))
	for _, r := range recorders {
		r.stop()
		list = append(list, r)
	}
	recordersMu.Unlock()

	// one deadline for all recorders, they stop in parallel
	timeout := time.After(3 * time.Second)
	for _, r := range list {
		if r.exited != nil {
			select {
			case <-r.exited:
			case <-timeout:
			}
		}
		r.mu.Lock()
		if r.cons != nil {
			_ = r.cons.Stop()
		}
		r.mu.Unlock()
	}
}
