package record

import (
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/AlexxIT/go2rtc/internal/streams"
	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/record"
)

const (
	stateConnecting = "connecting"
	stateRecording  = "recording"
	stateError      = "error"
)

type recorder struct {
	name   string
	query  string
	mode   string
	medias []*core.Media

	done     chan struct{}
	exited   chan struct{}
	stopOnce sync.Once

	mu    sync.Mutex
	cons  *record.Consumer
	state string
	err   error
	since time.Time
}

func (r *recorder) stop() {
	r.stopOnce.Do(func() {
		close(r.done)
	})
}

func (r *recorder) stopped() bool {
	select {
	case <-r.done:
		return true
	default:
		return false
	}
}

func (r *recorder) run() {
	if r.exited != nil {
		defer close(r.exited)
	}
	log.Info().Msgf("[record] start stream=%s", r.name)

	for retry := 0; ; retry++ {
		err := r.record()
		if r.stopped() {
			log.Info().Msgf("[record] stop stream=%s", r.name)
			return
		}

		log.Warn().Err(err).Msgf("[record] stream=%s retry=%d", r.name, retry)
		r.setState(stateError, nil, err)

		select {
		case <-r.done:
			log.Info().Msgf("[record] stop stream=%s", r.name)
			return
		case <-time.After(retryTimeout(retry)):
		}
	}
}

// record attaches consumer to the stream and blocks until stop or stall
func (r *recorder) record() error {
	stream := streams.Get(r.name)
	if stream == nil {
		return errors.New("stream not found")
	}

	cons := record.NewConsumer(r.medias, record.Options{
		Stream:          r.name,
		Dir:             cat.streamDir(r.name),
		SegmentDuration: cfg.segment,
		OnSegment:       cat.update,
		OnError: func(err error) {
			log.Error().Err(err).Msgf("[record] stream=%s", r.name)
		},
		// events mode: the pre-buffer is in memory, the disk is written only during events
		Hold:      r.mode == modeEvents,
		PreBuffer: cfg.pre,
	})

	r.setState(stateConnecting, cons, nil)

	if err := stream.AddConsumer(cons); err != nil {
		return err
	}
	if r.stopped() {
		stream.RemoveConsumer(cons)
		return nil
	}

	cons.Start()
	started := time.Now()

	r.setState(stateRecording, cons, nil)
	r.updateHold()

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	// events mode: check events state (start, end + post time)
	var holdTicker <-chan time.Time
	if r.mode == modeEvents {
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		holdTicker = t.C
	}

	for {
		select {
		case <-r.done:
			stream.RemoveConsumer(cons)
			return nil
		case <-holdTicker:
			r.updateHold()
		case now := <-ticker.C:
			last := cons.LastPacket()
			if last.IsZero() {
				last = started
			}
			// the producer reconnects by itself, but it can stay "connected" to a dead camera
			if now.Sub(last) > cfg.stallTimeout {
				stream.RemoveConsumer(cons)
				return errors.New("no data for " + cfg.stallTimeout.String())
			}
		}
	}
}

// updateHold writes to disk while any event is active (events mode)
func (r *recorder) updateHold() {
	if r.mode != modeEvents {
		return
	}
	r.mu.Lock()
	cons := r.cons
	r.mu.Unlock()
	if cons != nil {
		cons.SetHold(!events.active(r.name, time.Now()))
	}
}

func (r *recorder) setState(state string, cons *record.Consumer, err error) {
	r.mu.Lock()
	if r.state != state {
		r.since = time.Now()
	}
	r.state = state
	r.cons = cons
	r.err = err
	r.mu.Unlock()
}

func (r *recorder) MarshalJSON() ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	v := struct {
		Query      string          `json:"query"`
		Mode       string          `json:"mode"`
		State      string          `json:"state"`
		Holding    bool            `json:"holding,omitempty"`
		Since      time.Time       `json:"since"`
		Error      string          `json:"error,omitempty"`
		LastPacket *time.Time      `json:"last_packet,omitempty"`
		Segment    *record.Segment `json:"segment,omitempty"`
	}{
		Query: r.query,
		Mode:  r.mode,
		State: r.state,
		Since: r.since,
	}

	if r.err != nil {
		v.Error = r.err.Error()
	}

	if r.cons != nil {
		if last := r.cons.LastPacket(); !last.IsZero() {
			v.LastPacket = &last
		}
		v.Segment = r.cons.Current()
		v.Holding = r.mode == modeEvents && r.cons.Holding()
	}

	return json.Marshal(v)
}

func retryTimeout(retry int) time.Duration {
	switch {
	case retry < 5:
		return 5 * time.Second
	case retry < 10:
		return 15 * time.Second
	default:
		return time.Minute
	}
}
