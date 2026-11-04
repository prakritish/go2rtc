package streams

import (
	"encoding/json"
	"sync"
	"sync/atomic"

	"github.com/AlexxIT/go2rtc/pkg/core"
)

// Stream - producers and consumers are published as immutable slices behind
// atomic pointers. Writers hold mu and store a freshly allocated slice
// (copy-on-write); readers do a single atomic load. This drops the reference
// to a removed consumer immediately, instead of leaving it live in the
// [len:cap] tail of a shared backing array where the GC still treats it as
// reachable from the global streams map.
type Stream struct {
	producers atomic.Pointer[[]*Producer]
	consumers atomic.Pointer[[]core.Consumer]
	mu        sync.Mutex
	pending   atomic.Int32
}

// producerList - current producers, or nil. Safe without holding mu.
func (s *Stream) producerList() []*Producer {
	if p := s.producers.Load(); p != nil {
		return *p
	}
	return nil
}

// consumerList - current consumers, or nil. Safe without holding mu.
func (s *Stream) consumerList() []core.Consumer {
	if p := s.consumers.Load(); p != nil {
		return *p
	}
	return nil
}

// appendProducer - copy-on-write append. Caller must hold mu.
func (s *Stream) appendProducer(prod *Producer) {
	old := s.producerList()
	next := make([]*Producer, len(old)+1)
	copy(next, old)
	next[len(old)] = prod
	s.producers.Store(&next)
}

// appendConsumer - copy-on-write append. Caller must hold mu.
func (s *Stream) appendConsumer(cons core.Consumer) {
	old := s.consumerList()
	next := make([]core.Consumer, len(old)+1)
	copy(next, old)
	next[len(old)] = cons
	s.consumers.Store(&next)
}

func NewStream(source any) *Stream {
	switch source := source.(type) {
	case string:
		s := new(Stream)
		s.appendProducer(NewProducer(source))
		return s
	case []string:
		s := new(Stream)
		for _, str := range source {
			s.appendProducer(NewProducer(str))
		}
		return s
	case []any:
		s := new(Stream)
		for _, src := range source {
			str, ok := src.(string)
			if !ok {
				log.Error().Msgf("[stream] NewStream: Expected string, got %v", src)
				continue
			}
			s.appendProducer(NewProducer(str))
		}
		return s
	case map[string]any:
		return NewStream(source["url"])
	case nil:
		return new(Stream)
	default:
		panic(core.Caller())
	}
}

func (s *Stream) Sources() []string {
	producers := s.producerList()
	sources := make([]string, 0, len(producers))
	for _, prod := range producers {
		sources = append(sources, prod.url)
	}
	return sources
}

func (s *Stream) SetSource(source string) {
	for _, prod := range s.producerList() {
		prod.SetSource(source)
	}
}

func (s *Stream) RemoveConsumer(cons core.Consumer) {
	_ = cons.Stop()

	s.mu.Lock()
	old := s.consumerList()
	for i, consumer := range old {
		if consumer == cons {
			next := make([]core.Consumer, 0, len(old)-1)
			next = append(next, old[:i]...)
			next = append(next, old[i+1:]...)
			s.consumers.Store(&next)
			break
		}
	}
	s.mu.Unlock()

	s.stopProducers()
}

func (s *Stream) AddProducer(prod core.Producer) {
	producer := &Producer{conn: prod, state: stateExternal, url: "external"}
	s.mu.Lock()
	s.appendProducer(producer)
	s.mu.Unlock()
}

func (s *Stream) RemoveProducer(prod core.Producer) {
	s.mu.Lock()
	old := s.producerList()
	for i, producer := range old {
		if producer.conn == prod {
			next := make([]*Producer, 0, len(old)-1)
			next = append(next, old[:i]...)
			next = append(next, old[i+1:]...)
			s.producers.Store(&next)
			break
		}
	}
	s.mu.Unlock()
}

func (s *Stream) stopProducers() {
	if s.pending.Load() > 0 {
		log.Trace().Msg("[streams] skip stop pending producer")
		return
	}

	s.mu.Lock()
producers:
	for _, producer := range s.producerList() {
		for _, track := range producer.receivers {
			if len(track.Senders()) > 0 {
				continue producers
			}
		}
		for _, track := range producer.senders {
			if len(track.Senders()) > 0 {
				continue producers
			}
		}
		producer.stop()
	}
	s.mu.Unlock()
}

func (s *Stream) MarshalJSON() ([]byte, error) {
	var info = struct {
		Producers []*Producer     `json:"producers"`
		Consumers []core.Consumer `json:"consumers"`
	}{
		Producers: s.producerList(),
		Consumers: s.consumerList(),
	}
	return json.Marshal(info)
}
