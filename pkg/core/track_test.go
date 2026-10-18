package core

import (
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSenser(t *testing.T) {
	recv := make(chan *Packet) // blocking receiver

	sender := NewSender(nil, &Codec{})
	sender.Output = func(packet *Packet) {
		recv <- packet
	}
	require.Equal(t, "new", sender.State())

	sender.Start()
	require.Equal(t, "connected", sender.State())

	sender.Input(&Packet{})
	sender.Input(&Packet{})

	require.Equal(t, 2, sender.Packets)
	require.Equal(t, 0, sender.Drops)

	// important to read one before close
	// because goroutine in Start() can run with nil chan
	// it's OK in real life, but bad for test
	_, ok := <-recv
	require.True(t, ok)

	sender.Close()
	require.Equal(t, "closed", sender.State())

	sender.Input(&Packet{})

	require.Equal(t, 2, sender.Packets)
	require.Equal(t, 1, sender.Drops)

	// read 2nd
	_, ok = <-recv
	require.True(t, ok)

	// read 3rd
	select {
	case <-recv:
		ok = true
	default:
		ok = false
	}
	require.False(t, ok)
}

// TestReceiverChilds: childs are changed while the receiver writes packets (run with -race),
// the receiver doesn't allocate per packet
func TestReceiverChilds(t *testing.T) {
	recv := NewReceiver(nil, &Codec{})

	var n atomic.Int32
	newChild := func() *Node {
		return &Node{Input: func(*Packet) { n.Add(1) }}
	}
	recv.AppendChild(newChild())

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 1000; i++ {
			child := newChild()
			recv.AppendChild(child)
			recv.RemoveChild(child)
		}
	}()
	for i := 0; i < 1000; i++ {
		recv.Input(&Packet{})
	}
	<-done

	require.Len(t, recv.childList(), 1)
	require.GreaterOrEqual(t, n.Load(), int32(1000))

	packet := &Packet{}
	allocs := testing.AllocsPerRun(100, func() { recv.Input(packet) })
	require.Zero(t, allocs)
}
