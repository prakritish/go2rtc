package core

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/rtp"
)

//type Packet struct {
//	Payload     []byte
//	Timestamp   uint32 // PTS if DTS == 0 else DTS
//	Composition uint32 // CTS = PTS-DTS (for support B-frames)
//	Sequence    uint16
//}

type Packet = rtp.Packet

// HandlerFunc - process input packets (just like http.HandlerFunc)
type HandlerFunc func(packet *Packet)

// Filter - a decorator for any HandlerFunc
type Filter func(handler HandlerFunc) HandlerFunc

// Node - Receiver or Sender or Filter (transform)
type Node struct {
	Codec  *Codec
	Input  HandlerFunc
	Output HandlerFunc

	id     uint32
	childs atomic.Pointer[[]*Node] // immutable slice: writers (holding mu) store a fresh one, readers load it without the lock
	parent *Node

	mu sync.Mutex

	senderReport atomic.Pointer[SenderReport]
}

// SenderReport - mapping of the RTP time to the sender wall clock (RTCP sender report).
// Tracks of one source can be synchronized with it (ex. audio and video).
type SenderReport struct {
	NTP      time.Time // sender wall clock
	RTPTime  uint32    // RTP timestamp for this time
	Received time.Time // local time when the report was received
}

// Time returns the sender wall clock time of the RTP timestamp
func (r *SenderReport) Time(ts uint32, clockRate uint32) time.Time {
	ticks := int64(int32(ts - r.RTPTime))
	return r.NTP.Add(time.Duration(ticks) * time.Second / time.Duration(clockRate))
}

// SetSenderReport - should be called by the producer for the receiver
func (n *Node) SetSenderReport(sr *SenderReport) {
	n.senderReport.Store(sr)
}

// SenderReport returns the last sender report for the receiver (nil if none)
func (n *Node) SenderReport() *SenderReport {
	return n.senderReport.Load()
}

func (n *Node) WithParent(parent *Node) *Node {
	parent.AppendChild(n)
	return n
}

func (n *Node) AppendChild(child *Node) {
	n.mu.Lock()
	old := n.childList()
	next := make([]*Node, len(old)+1)
	copy(next, old)
	next[len(old)] = child
	n.childs.Store(&next)
	n.mu.Unlock()

	child.mu.Lock()
	child.parent = n
	child.mu.Unlock()
}

// Parent returns the parent node, it can be changed on producer reconnect (MoveNode)
func (n *Node) Parent() *Node {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.parent
}

// childList returns the current child nodes, the slice must not be changed.
// It is a single atomic load, so it is safe to call with or without holding mu.
func (n *Node) childList() []*Node {
	if p := n.childs.Load(); p != nil {
		return *p
	}
	return nil
}

func (n *Node) RemoveChild(child *Node) {
	n.mu.Lock()
	old := n.childList()
	for i, ch := range old {
		if ch == child {
			next := make([]*Node, 0, len(old)-1)
			next = append(next, old[:i]...)
			next = append(next, old[i+1:]...)
			n.childs.Store(&next)
			break
		}
	}
	n.mu.Unlock()
}

func (n *Node) Close() {
	if parent := n.Parent(); parent != nil {
		parent.RemoveChild(n)

		if len(parent.childList()) == 0 {
			parent.Close()
		}
	} else {
		for _, child := range n.childList() {
			child.Close()
		}
	}
}

func MoveNode(dst, src *Node) {
	src.mu.Lock()
	childs := src.childList()
	src.childs.Store(nil)
	src.mu.Unlock()

	dst.mu.Lock()
	dst.childs.Store(&childs)
	dst.mu.Unlock()

	for _, child := range childs {
		child.mu.Lock()
		child.parent = dst
		child.mu.Unlock()
	}
}
