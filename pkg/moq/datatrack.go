// Copyright 2026 LiveKit, Inc.
// SPDX-License-Identifier: Apache-2.0

package moq

import (
	"bytes"
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
	done     bool
}

type dataAssembler struct {
	highest uint64
	groups  [dataGroupWindow]*dataGroup
}

func (a *dataAssembler) push(d *message.Datagram) []byte {
	n := len(d.Payload)
	if d.HasStatus || n == 0 || n > dataChunkSize || d.Object >= maxDataChunks {
		return nil
	}
	if !d.EndOfGroup && (n != dataChunkSize || d.Object == maxDataChunks-1) {
		return nil
	}
	if d.Group < a.highest && a.highest-d.Group >= dataGroupWindow {
		return nil
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
		return nil
	}
	if d.EndOfGroup {
		if g.received>>(i+1) != 0 {
			return nil
		}
		if i == 0 {
			g.done = true
			return bytes.Clone(d.Payload)
		}
		g.last = i
	}
	g.chunks[i] = bytes.Clone(d.Payload)
	g.received |= bit
	if g.last < 0 || g.received != (uint64(1)<<(g.last+1))-1 {
		return nil
	}
	packet := bytes.Join(g.chunks[:g.last+1], nil)
	*g = dataGroup{id: d.Group, done: true}
	return packet
}

type dataUpload struct {
	peer      *moqPeer
	track     types.DataTrack
	mu        sync.Mutex
	assembler dataAssembler
}

func (p *moqPeer) acceptDataUpload(r *session.Request, sid livekit.TrackID) bool {
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
		r.SetHandlers(nil, &dataUpload{peer: p, track: track})
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
	packet := u.assembler.push(d)
	u.mu.Unlock()
	if packet != nil {
		u.peer.participant.ReceiveMoQDataTrack(u.peer.generation, u.track, packet, mono.UnixNano())
	}
}

func (*dataUpload) HandleFetch(_ *session.Request, in *session.IncomingFetch) {
	in.Cancel(message.ResetCancelled)
}
func (*dataUpload) HandleSubgroup(_ *session.Request, in *session.IncomingSubgroup) {
	in.Cancel(message.ResetCancelled)
}

type dataDownload struct {
	request *session.Request
	queue   chan []byte
}

// Each active subscriber handle has an independent, bounded packet queue.
func (p *moqPeer) SendDataTrackMessage(packet []byte) error {
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
	// All producers hold p.mu. The reader can only make more space.
	if len(sub.queue) < cap(sub.queue) {
		sub.queue <- bytes.Clone(packet)
	}
	return nil
}

func (p *moqPeer) serveData(r *session.Request, sid livekit.TrackID) bool {
	track := p.participant.GetSubscribedDataTrack(sid)
	if track == nil || !p.participant.CanSubscribe() || !p.participant.IsMoQAttachmentCurrent(p.generation) {
		return false
	}
	handle := track.Handle()
	download := &dataDownload{request: r, queue: make(chan []byte, 32)}
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
	for group := uint64(0); ; group++ {
		select {
		case <-r.Context().Done():
			return true
		case <-sub.update:
			endMoQSubscriptionOnUpdate(r)
			return true
		case packet := <-download.queue:
			if !p.participant.IsMoQAttachmentCurrent(p.generation) || !p.participant.CanSubscribe() || p.participant.GetSubscribedDataTrack(sid) != track {
				r.Cancel(message.ResetCancelled)
				return true
			}
			for start := 0; start < len(packet); start += dataChunkSize {
				end := min(start+dataChunkSize, len(packet))
				d := &message.Datagram{
					Group: group, Object: uint64(start / dataChunkSize), EndOfGroup: end == len(packet),
					DefaultPriority: true, Payload: packet[start:end],
				}
				if err := r.SendDatagram(d); err != nil {
					break
				}
			}
		}
	}
}
