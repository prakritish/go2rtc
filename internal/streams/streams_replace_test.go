package streams

import (
	"sync/atomic"
	"testing"

	"github.com/AlexxIT/go2rtc/pkg/core"
)

// stopConsumer counts Stop calls, like a client that gets disconnected.
type stopConsumer struct{ stops atomic.Int32 }

func (*stopConsumer) GetMedias() []*core.Media                                { return nil }
func (*stopConsumer) AddTrack(*core.Media, *core.Codec, *core.Receiver) error { return nil }
func (c *stopConsumer) Stop() error                                           { c.stops.Add(1); return nil }

func replaceTestStream(t *testing.T, name string, sources ...string) *Stream {
	t.Helper()
	HandleFunc("replace", func(string) (core.Producer, error) { return nil, nil }) // bypass HasProducer
	stream, err := Replace(name, sources...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { Delete(name) })
	return stream
}

func TestReplaceSameSourcesKeepsStream(t *testing.T) {
	old := replaceTestStream(t, "replace_same", "replace://cam1")
	cons := new(stopConsumer)
	old.AddInternalConsumer(cons)

	stream := replaceTestStream(t, "replace_same", "replace://cam1")

	if stream != old {
		t.Fatal("stream with the same sources was replaced")
	}
	if n := cons.stops.Load(); n != 0 {
		t.Fatalf("consumer of an unchanged stream was stopped %d times", n)
	}
}

func TestReplaceNewSourcesClosesOldStream(t *testing.T) {
	old := replaceTestStream(t, "replace_new", "replace://cam1")
	cons := new(stopConsumer)
	old.AddInternalConsumer(cons)

	stream := replaceTestStream(t, "replace_new", "replace://cam2")

	if stream == old || Get("replace_new") != stream {
		t.Fatal("stream was not replaced")
	}
	if got := stream.Sources(); len(got) != 1 || got[0] != "replace://cam2" {
		t.Fatalf("sources = %v", got)
	}
	// the client must reconnect to reach the new source
	if n := cons.stops.Load(); n != 1 {
		t.Fatalf("consumer of the replaced stream was stopped %d times, want 1", n)
	}
}

func TestReplaceKeepsLinkedStream(t *testing.T) {
	old := replaceTestStream(t, "replace_link1", "replace://cam1")
	streamsMu.Lock()
	streams["replace_link2"] = old // alias, like Patch makes
	streamsMu.Unlock()
	t.Cleanup(func() { Delete("replace_link2") })
	cons := new(stopConsumer)
	old.AddInternalConsumer(cons)

	replaceTestStream(t, "replace_link1", "replace://cam2")

	if Get("replace_link2") != old {
		t.Fatal("linked name lost its stream")
	}
	if n := cons.stops.Load(); n != 0 {
		t.Fatalf("consumer of a still linked stream was stopped %d times", n)
	}
}

func TestRemoveClosesStream(t *testing.T) {
	old := replaceTestStream(t, "replace_remove", "replace://cam1")
	cons := new(stopConsumer)
	old.AddInternalConsumer(cons)

	Remove("replace_remove")

	if Get("replace_remove") != nil {
		t.Fatal("stream was not removed")
	}
	if n := cons.stops.Load(); n != 1 {
		t.Fatalf("consumer of the removed stream was stopped %d times, want 1", n)
	}
}

func TestReplaceRejectsUnsupportedSource(t *testing.T) {
	if _, err := Replace("replace_bad", "nope://cam1"); err == nil {
		Delete("replace_bad")
		t.Fatal("unsupported source accepted")
	}
	if Get("replace_bad") != nil {
		t.Fatal("stream created for an unsupported source")
	}
}
