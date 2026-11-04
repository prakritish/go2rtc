package core

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestNodeRemoveChildReleasesReference is the leak regression test.
//
// The delete idiom `append(s[:i], s[i+1:]...)` shifts elements left but leaves
// the final element live in the [len:cap] tail of the backing array. The GC
// scans that array by capacity, so the removed child stays reachable and is
// never collected. On a Receiver reachable from the global streams map that
// retention is permanent.
func TestNodeRemoveChildReleasesReference(t *testing.T) {
	r := NewReceiver(&Media{}, &Codec{})

	for i := 0; i < 8; i++ {
		r.AppendChild(&Node{id: NewID()})
	}

	var collected atomic.Bool
	func() {
		victim := &Node{id: NewID()}
		runtime.SetFinalizer(victim, func(*Node) { collected.Store(true) })
		r.AppendChild(victim)
		r.RemoveChild(victim)
	}()

	for i := 0; i < 10 && !collected.Load(); i++ {
		runtime.GC()
		time.Sleep(20 * time.Millisecond)
	}

	if !collected.Load() {
		t.Fatal("removed child is still reachable: slice retained it past len")
	}
}

// TestNodeConcurrentInputAndRemoveChild is the race regression test.
//
// Receiver.Input ranges over childs on the hot packet path without holding mu.
// Any writer that mutates the shared backing array, or that publishes a new
// slice header non-atomically, can be observed mid-update by that reader.
// Run with -race.
func TestNodeConcurrentInputAndRemoveChild(t *testing.T) {
	r := NewReceiver(&Media{}, &Codec{})

	var delivered atomic.Int64
	newChild := func() *Node {
		n := &Node{id: NewID()}
		n.Input = func(*Packet) { delivered.Add(1) }
		return n
	}

	live := make([]*Node, 0, 64)
	for i := 0; i < 64; i++ {
		c := newChild()
		r.AppendChild(c)
		live = append(live, c)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Single packet pump: Receiver.Input mutates r.Bytes/r.Packets without
	// atomics, so more than one pump would trip the detector on a pre-existing
	// unrelated race. One reader is enough to prove the childs race.
	wg.Add(1)
	go func() {
		defer wg.Done()
		pkt := &Packet{}
		for {
			select {
			case <-stop:
				return
			default:
				r.Input(pkt)
			}
		}
	}()

	// Churn children while packets flow.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				victim := live[0]
				live = live[1:]
				r.RemoveChild(victim)

				fresh := newChild()
				r.AppendChild(fresh)
				live = append(live, fresh)
			}
		}
	}()

	time.Sleep(3 * time.Second)
	close(stop)
	wg.Wait()

	if delivered.Load() == 0 {
		t.Fatal("no packets were delivered")
	}
	t.Logf("delivered %d packets while churning children", delivered.Load())
}
