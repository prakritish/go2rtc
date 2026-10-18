package record

import (
	"encoding/binary"
	"sync/atomic"

	"github.com/AlexxIT/go2rtc/pkg/aac"
	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/pion/rtp"
)

// aacDepay - like aac.RTPDepay, but keeps RTP timestamps, they are needed for the synchronization
// by RTCP sender reports (aac.RTPDepay makes own timestamps from zero).
// If the camera sends broken timestamps, it falls back to own timestamps like aac.RTPDepay.
type aacDepay struct {
	handler core.HandlerFunc

	started bool
	next    uint32 // expected RTP timestamp of the next packet
	counter uint32 // own timestamps (fallback)
	bad     int    // broken timestamps in a row

	broken atomic.Bool // timestamps are broken, sender reports can't be used
}

// aacMaxBad - broken timestamps in a row before the fallback
const aacMaxBad = 3

func (d *aacDepay) Handle(packet *rtp.Packet) {
	payload := packet.Payload
	if len(payload) < 2 {
		return
	}

	ts := d.timestamp(packet.Timestamp)

	// support ONLY 2 bytes header size (RFC 3640, mode=AAC-hbr)
	headersSize := int(binary.BigEndian.Uint16(payload) >> 3)
	if len(payload) < 2+headersSize {
		// some cameras send ADTS without RTP AAC headers (same as aac.RTPDepay)
		if aac.IsADTS(payload) {
			d.emit(packet, payload[aac.ADTSHeaderSize:], ts)
			d.advance(packet.Timestamp, 1)
		}
		return
	}

	headers := payload[2 : 2+headersSize]
	units := payload[2+headersSize:]

	var n uint32
	for len(headers) >= 2 {
		size := int(binary.BigEndian.Uint16(headers) >> 3)
		if len(units) < size {
			break
		}
		unit := units[:size]
		headers, units = headers[2:], units[size:]

		if aac.IsADTS(unit) {
			unit = unit[aac.ADTSHeaderSize:]
		}

		// the RTP timestamp is for the first unit, next units follow it
		d.emit(packet, unit, ts+n*aac.AUTime)
		n++
	}

	d.advance(packet.Timestamp, n)
}

// reset - new RTP session (source reconnect)
func (d *aacDepay) reset() {
	d.started = false
	d.bad = 0
	d.broken.Store(false)
}

// timestamp checks the RTP timestamp and returns the timestamp for the first unit
func (d *aacDepay) timestamp(ts uint32) uint32 {
	if !d.started {
		d.started = true
		d.counter = ts
		return ts
	}

	if d.broken.Load() {
		return d.counter
	}

	// timestamps can jump forward only by whole units (lost packets)
	if delta := int32(ts - d.next); delta < 0 || delta%aac.AUTime != 0 {
		// wrong timestamp: continue own timestamps, so the output is always continuous
		if d.bad++; d.bad >= aacMaxBad {
			d.broken.Store(true)
		}
		return d.counter
	}

	d.bad = 0
	d.counter = ts
	return ts
}

func (d *aacDepay) advance(ts, units uint32) {
	d.next = ts + units*aac.AUTime
	d.counter += units * aac.AUTime
}

func (d *aacDepay) emit(packet *rtp.Packet, payload []byte, ts uint32) {
	clone := *packet
	clone.Version = aac.RTPPacketVersionAAC
	clone.Timestamp = ts
	clone.Payload = payload
	d.handler(&clone)
}
