// Copyright 2026 LiveKit, Inc.
// SPDX-License-Identifier: Apache-2.0

package moq

import (
	"bytes"
	"cmp"
	"context"
	"sync"

	"github.com/livekit/protocol/datatrack"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/utils/mono"
	"github.com/trickstercache/trickster/v2/pkg/moq/message"
	"github.com/trickstercache/trickster/v2/pkg/moq/session"

	"github.com/livekit/livekit-server/pkg/rtc"
	"github.com/livekit/livekit-server/pkg/rtc/types"
)

// One group is one serialized LiveKit packet. Each object is a datagram-sized
// piece. The final object sets EndOfGroup. A missing group never blocks another.
const (
	dataChunkSize   = 1024
	maxDataChunks   = 64
	dataGroupWindow = 16
)

type dataGroup struct {
	id       uint64
	chunks   [maxDataChunks][]byte
	received uint64
	last     int
	priority uint8
	done     bool
}

type dataAssembler struct {
	highest uint64
	groups  [dataGroupWindow]*dataGroup
}

func (a *dataAssembler) push(d *message.Datagram, defaultPriority uint8) ([]byte, uint8) {
	n := len(d.Payload)
	if d.HasStatus || n == 0 || n > dataChunkSize || d.Object >= maxDataChunks {
		return nil, 0
	}
	if !d.EndOfGroup && (n != dataChunkSize || d.Object == maxDataChunks-1) {
		return nil, 0
	}
	if d.Group < a.highest && a.highest-d.Group >= dataGroupWindow {
		return nil, 0
	}
	a.highest = max(a.highest, d.Group)
	slot := d.Group % dataGroupWindow
	g := a.groups[slot]
	if g == nil {
		g = &dataGroup{last: -1, id: d.Group}
		a.groups[slot] = g
	} else if g.id != d.Group {
		*g = dataGroup{last: -1, id: d.Group}
	}
	i := int(d.Object)
	bit := uint64(1) << i
	if g.done || g.received&bit != 0 || (g.last >= 0 && (i > g.last || d.EndOfGroup)) {
		return nil, 0
	}
	// The first chunk supplies the packet's priority even when it arrives last.
	if i == 0 {
		g.priority = d.Priority
		if d.DefaultPriority {
			g.priority = defaultPriority
		}
	}
	if d.EndOfGroup {
		if g.received>>(i+1) != 0 {
			return nil, 0
		}
		if i == 0 {
			g.done = true
			return bytes.Clone(d.Payload), g.priority
		}
		g.last = i
	}
	g.chunks[i] = bytes.Clone(d.Payload)
	g.received |= bit
	if g.last < 0 || g.received != (uint64(1)<<(g.last+1))-1 {
		return nil, 0
	}
	packet := bytes.Join(g.chunks[:g.last+1], nil)
	priority := g.priority
	*g = dataGroup{id: d.Group, done: true}
	return packet, priority
}

type dataUpload struct {
	peer            *moqPeer
	track           types.DataTrack
	defaultPriority uint8
	mu              sync.Mutex
	assembler       dataAssembler
}

func (p *moqPeer) acceptDataUpload(r *session.Request, sid livekit.TrackID, defaultPriority uint8) bool {
	if !p.participant.CanPublishData() || !p.participant.IsMoQAttachmentCurrent(p.generation) {
		return false
	}
	for _, track := range p.participant.GetPublishedDataTracks() {
		if track.ID() != sid {
			continue
		}
		p.mu.Lock()
		if prev := p.uploads[sid]; prev != nil && prev.Context().Err() == nil {
			p.mu.Unlock()
			return false
		}
		p.uploads[sid] = r
		p.mu.Unlock()
		defer func() {
			p.mu.Lock()
			if p.uploads[sid] == r {
				delete(p.uploads, sid)
			}
			p.mu.Unlock()
		}()
		r.SetHandlers(nil, &dataUpload{peer: p, track: track, defaultPriority: defaultPriority})
		if err := r.Send(&message.RequestOK{}); err == nil {
			<-r.Context().Done()
		}
		return true
	}
	return false
}

func (u *dataUpload) HandleDatagram(r *session.Request, d *message.Datagram) {
	if r.Context().Err() != nil || !u.peer.participant.CanPublishData() {
		return
	}
	u.mu.Lock()
	packet, priority := u.assembler.push(d, u.defaultPriority)
	u.mu.Unlock()
	if packet != nil {
		u.peer.participant.ReceiveMoQDataTrack(u.peer.generation, u.track, packet, mono.UnixNano(), priority)
	}
}

func (*dataUpload) HandleFetch(_ *session.Request, in *session.IncomingFetch) {
	in.Cancel(message.ResetCancelled)
}
func (*dataUpload) HandleSubgroup(_ *session.Request, in *session.IncomingSubgroup) {
	in.Cancel(message.ResetCancelled)
}

type queuedDataPacket struct {
	payload  []byte
	priority uint8
	sequence uint64
}

type dataDownload struct {
	request            *session.Request
	track              types.DataDownTrack
	sid                livekit.TrackID
	subscriberPriority uint8
	ready              bool
	group              uint64
	queue              []queuedDataPacket
}

// All tracks for this subscriber share one QUIC send queue. Select a packet
// before handing it to QUIC, while its priority can still affect send order.
func (p *moqPeer) nextDataPacket() (*dataDownload, queuedDataPacket, uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var next *dataDownload
	var selected queuedDataPacket
	for _, sub := range p.downloads {
		if !sub.ready || len(sub.queue) == 0 {
			continue
		}
		head := sub.queue[0]
		if next == nil || cmp.Or(
			cmp.Compare(sub.subscriberPriority, next.subscriberPriority),
			cmp.Compare(head.priority, selected.priority),
			cmp.Compare(head.sequence, selected.sequence),
		) < 0 {
			next, selected = sub, head
		}
	}
	if next == nil {
		return nil, queuedDataPacket{}, 0
	}
	next.queue[0] = queuedDataPacket{}
	next.queue = next.queue[1:]
	group := next.group
	next.group++
	return next, selected, group
}

func (p *moqPeer) notifyDataWriter() {
	select {
	case p.dataReady <- struct{}{}:
	default:
	}
}

// Each subscriber handle has an independent bounded queue.
func (p *moqPeer) SendDataTrackMessage(packet []byte, priority uint8) error {
	var h datatrack.Header
	if _, err := h.Unmarshal(packet); err != nil {
		return err
	}
	if len(packet) > maxDataChunks*dataChunkSize {
		return rtc.ErrDataChannelUnavailable
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	sub := p.downloads[h.Handle]
	if sub == nil || sub.request.Context().Err() != nil {
		return rtc.ErrDataChannelUnavailable
	}
	if len(sub.queue) < 32 {
		sub.queue = append(sub.queue, queuedDataPacket{payload: bytes.Clone(packet), priority: priority, sequence: p.dataSequence})
		p.dataSequence++
		p.notifyDataWriter()
	}
	return nil
}

func (p *moqPeer) writeData(ctx context.Context) {
	for ctx.Err() == nil {
		sub, packet, group := p.nextDataPacket()
		if sub == nil {
			select {
			case <-ctx.Done():
				return
			case <-p.dataReady:
				continue
			}
		}
		if sub.request.Context().Err() != nil {
			continue
		}
		if !p.participant.IsMoQAttachmentCurrent(p.generation) || !p.participant.CanSubscribe() || p.participant.GetSubscribedDataTrack(sub.sid) != sub.track {
			sub.request.Cancel(message.ResetCancelled)
			continue
		}
		for start := 0; start < len(packet.payload); start += dataChunkSize {
			end := min(start+dataChunkSize, len(packet.payload))
			d := &message.Datagram{
				Group: group, Object: uint64(start / dataChunkSize), EndOfGroup: end == len(packet.payload),
				Priority: packet.priority, DefaultPriority: packet.priority == message.DefaultPublisherPriority,
				Payload: packet.payload[start:end],
			}
			if err := sub.request.SendDatagram(d); err != nil {
				break
			}
		}
	}
}

func (p *moqPeer) serveData(r *session.Request, sid livekit.TrackID, subscriberPriority uint8) bool {
	track := p.participant.GetSubscribedDataTrack(sid)
	if track == nil || !p.participant.CanSubscribe() || !p.participant.IsMoQAttachmentCurrent(p.generation) {
		return false
	}
	handle := track.Handle()
	download := &dataDownload{request: r, sid: sid, track: track, subscriberPriority: subscriberPriority}
	p.mu.Lock()
	if prev := p.downloads[handle]; prev != nil && prev.request.Context().Err() == nil {
		p.mu.Unlock()
		return false
	}
	p.downloads[handle] = download
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		if p.downloads[handle] == download {
			delete(p.downloads, handle)
		}
		p.mu.Unlock()
	}()
	sub, err := acceptMoQSubscription(r)
	if err != nil {
		return true
	}
	p.mu.Lock()
	download.ready = true
	p.mu.Unlock()
	p.notifyDataWriter()
	select {
	case <-r.Context().Done():
	case <-sub.update:
		endMoQSubscriptionOnUpdate(r)
	}
	return true
}
