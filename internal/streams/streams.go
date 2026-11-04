package streams

import (
	"errors"
	"net/url"
	"slices"
	"sync"
	"time"

	"github.com/AlexxIT/go2rtc/internal/api"
	"github.com/AlexxIT/go2rtc/internal/app"
	"github.com/rs/zerolog"
)

func Init() {
	var cfg struct {
		Streams map[string]any    `yaml:"streams"`
		Publish map[string]any    `yaml:"publish"`
		Preload map[string]string `yaml:"preload"`
	}

	app.LoadConfig(&cfg)

	log = app.GetLogger("streams")

	for name, item := range cfg.Streams {
		streams[name] = NewStream(item)
	}

	api.HandleFunc("api/streams", apiStreams)
	api.HandleFunc("api/streams.dot", apiStreamsDOT)
	api.HandleFunc("api/preload", apiPreload)
	api.HandleFunc("api/schemes", apiSchemes)

	if cfg.Publish == nil && cfg.Preload == nil {
		return
	}

	time.AfterFunc(time.Second, func() {
		// range for nil map is OK
		for name, dst := range cfg.Publish {
			if stream := Get(name); stream != nil {
				Publish(stream, dst)
			}
		}
		for name, rawQuery := range cfg.Preload {
			if err := AddPreload(name, rawQuery); err != nil {
				log.Error().Err(err).Caller().Send()
			}
		}
	})
}

func New(name string, sources ...string) (*Stream, error) {
	for _, source := range sources {
		if !HasProducer(source) {
			return nil, errors.New("streams: source not supported")
		}

		if err := Validate(source); err != nil {
			return nil, err
		}
	}

	stream := NewStream(sources)

	streamsMu.Lock()
	streams[name] = stream
	streamsMu.Unlock()

	return stream, nil
}

func Patch(name string, source string) (*Stream, error) {
	streamsMu.Lock()
	defer streamsMu.Unlock()

	// check if source links to some stream name from go2rtc
	if u, err := url.Parse(source); err == nil && u.Scheme == "rtsp" && len(u.Path) > 1 {
		rtspName := u.Path[1:]
		if stream, ok := streams[rtspName]; ok {
			if streams[name] != stream {
				// link (alias) streams[name] to streams[rtspName]
				streams[name] = stream
			}
			return stream, nil
		}
	}

	if stream, ok := streams[source]; ok {
		if name != source {
			// link (alias) streams[name] to streams[source]
			streams[name] = stream
		}
		return stream, nil
	}

	// check if src has supported scheme
	if !HasProducer(source) {
		return nil, errors.New("streams: source not supported")
	}

	if err := Validate(source); err != nil {
		return nil, err
	}

	// check an existing stream with this name
	if stream, ok := streams[name]; ok {
		stream.SetSource(source)
		return stream, nil
	}

	// create new stream with this name
	stream := NewStream(source)
	streams[name] = stream
	return stream, nil
}

// Replace sets the sources of the stream with this name (PUT /api/streams).
// A stream that already has exactly these sources is kept, so its consumers are not touched.
// A replaced stream is closed: its consumers disconnect and reconnect to the new sources
// (the recorder and a preload re-attach by name). A stream that another name links to is
// not closed.
func Replace(name string, sources ...string) (*Stream, error) {
	for _, source := range sources {
		if !HasProducer(source) {
			return nil, errors.New("streams: source not supported")
		}
		if err := Validate(source); err != nil {
			return nil, err
		}
	}

	streamsMu.Lock()
	old := streams[name]
	if old != nil && slices.Equal(old.Sources(), sources) {
		streamsMu.Unlock()
		return old, nil
	}
	stream := NewStream(sources)
	streams[name] = stream
	closeOld := old != nil && !linked(old)
	streamsMu.Unlock()

	if closeOld {
		old.close()
	}
	if old != nil {
		repreload(name)
	}
	return stream, nil
}

// Remove deletes the stream with this name (DELETE /api/streams) and closes it,
// unless another name links to it.
func Remove(name string) {
	streamsMu.Lock()
	old := streams[name]
	delete(streams, name)
	closeOld := old != nil && !linked(old)
	streamsMu.Unlock()

	if closeOld {
		_ = DelPreload(name)
		old.close()
	}
}

// linked - some stream name still points to this stream, call with streamsMu locked
func linked(stream *Stream) bool {
	for _, s := range streams {
		if s == stream {
			return true
		}
	}
	return false
}

// repreload attaches the preload of a replaced stream to the new stream
func repreload(name string) {
	preloadsMu.Lock()
	p := preloads[name]
	preloadsMu.Unlock()
	if p == nil {
		return
	}
	if err := AddPreload(name, p.Query); err != nil {
		log.Error().Err(err).Caller().Send()
	}
}

func GetOrPatch(query url.Values) (*Stream, error) {
	// check if src param exists
	source := query.Get("src")
	if source == "" {
		return nil, errors.New("streams: source empty")
	}

	// check if src is stream name
	if stream := Get(source); stream != nil {
		return stream, nil
	}

	// check if name param provided
	if name := query.Get("name"); name != "" {
		return Patch(name, source)
	}

	// return new stream with src as name
	return Patch(source, source)
}

var log zerolog.Logger

// streams map

var streams = map[string]*Stream{}
var streamsMu sync.Mutex

func Get(name string) *Stream {
	streamsMu.Lock()
	defer streamsMu.Unlock()
	return streams[name]
}

func Delete(name string) {
	streamsMu.Lock()
	defer streamsMu.Unlock()
	delete(streams, name)
}

func GetAllNames() []string {
	streamsMu.Lock()
	names := make([]string, 0, len(streams))
	for name := range streams {
		names = append(names, name)
	}
	streamsMu.Unlock()
	return names
}

func GetAllSources() map[string][]string {
	streamsMu.Lock()
	sources := make(map[string][]string, len(streams))
	for name, stream := range streams {
		sources[name] = stream.Sources()
	}
	streamsMu.Unlock()
	return sources
}
