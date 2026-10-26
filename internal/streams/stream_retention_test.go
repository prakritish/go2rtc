package streams

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
)

// fakeConsumer is the smallest core.Consumer, enough to sit in Stream.consumers.
type fakeConsumer struct{ _ [1 << 16]byte }

func (*fakeConsumer) GetMedias() []*core.Media                                { return nil }
func (*fakeConsumer) AddTrack(*core.Media, *core.Codec, *core.Receiver) error { return nil }
func (*fakeConsumer) Stop() error                                             { return nil }

// collected reports whether the finalizer of the removed value ran after a few GC cycles.
func collected(flag *atomic.Bool) bool {
	for i := 0; i < 10 && !flag.Load(); i++ {
		runtime.GC()
		time.Sleep(20 * time.Millisecond)
	}
	return flag.Load()
}

// The delete idiom append(s[:i], s[i+1:]...) shifts elements left but leaves the
// last element live in the [len:cap] tail of the backing array. The GC scans the
// array by capacity, so a removed consumer stayed reachable from the global streams
// map for as long as the stream lived. Each consumer keeps its Senders, and each
// Sender captures a 512 KB depay buffer, which is how a busy server ran out of memory.
func TestRemoveConsumerReleasesReference(t *testing.T) {
	s := new(Stream)
	for i := 0; i < 8; i++ {
		s.AddInternalConsumer(new(fakeConsumer))
	}

	var released atomic.Bool
	func() {
		victim := new(fakeConsumer)
		runtime.SetFinalizer(victim, func(*fakeConsumer) { released.Store(true) })
		s.AddInternalConsumer(victim)
		s.RemoveConsumer(victim)
	}()

	// The stream must stay alive while we wait: it plays the part of the global
	// streams map. If it were garbage too, the consumer would be freed with it
	// and the test could not see the retention.
	ok := collected(&released)
	runtime.KeepAlive(s)
	if !ok {
		t.Fatal("removed consumer is still reachable from Stream: slice kept it past len")
	}
}

func TestRemoveInternalConsumerReleasesReference(t *testing.T) {
	s := new(Stream)
	for i := 0; i < 8; i++ {
		s.AddInternalConsumer(new(fakeConsumer))
	}

	var released atomic.Bool
	func() {
		victim := new(fakeConsumer)
		runtime.SetFinalizer(victim, func(*fakeConsumer) { released.Store(true) })
		s.AddInternalConsumer(victim)
		s.RemoveInternalConsumer(victim)
	}()

	// The stream must stay alive while we wait: it plays the part of the global
	// streams map. If it were garbage too, the consumer would be freed with it
	// and the test could not see the retention.
	ok := collected(&released)
	runtime.KeepAlive(s)
	if !ok {
		t.Fatal("removed consumer is still reachable from Stream: slice kept it past len")
	}
}

// Readers such as MarshalJSON and AddConsumer iterate the consumer and producer
// slices while other goroutines add and remove entries. Run with -race.
func TestConsumersConcurrentReadWrite(t *testing.T) {
	s := new(Stream)
	stop := make(chan struct{})
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_, _ = s.MarshalJSON()
				_ = s.Sources()
			}
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				c := new(fakeConsumer)
				s.AddInternalConsumer(c)
				s.RemoveInternalConsumer(c)
			}
		}
	}()

	time.Sleep(time.Second)
	close(stop)
	wg.Wait()
}
