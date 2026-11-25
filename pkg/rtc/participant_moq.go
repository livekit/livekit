// Copyright 2026 LiveKit, Inc.
// SPDX-License-Identifier: Apache-2.0

package rtc

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/livekit/livekit-server/pkg/moq/media"
	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/sfu/connectionquality"
	"github.com/livekit/protocol/codecs/mime"
	"github.com/livekit/protocol/datatrack"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/utils"
	"github.com/pion/webrtc/v4"
)

// MoQTransport is the data plane attached to an existing signaling participant.
type MoQTransport interface {
	types.DataTrackTransport
	PathQuality() (float32, livekit.ConnectionQuality)
	Close()
}

// MoQParticipantOptions binds an opted-in participant to the owning room's publications.
type MoQParticipantOptions struct {
	endpoint string
	tracks   *RoomTrackManager
}

func (r *Room) MoQParticipantOptions(endpoint string) *MoQParticipantOptions {
	return &MoQParticipantOptions{endpoint: endpoint, tracks: r.trackManager}
}

type moqAttachment struct {
	mu               sync.Mutex
	generation       uint64
	digest           [sha256.Size]byte
	expires          time.Time
	active           MoQTransport
	activeGeneration uint64
	closed           bool
	options          *MoQParticipantOptions
	subscriptions    map[livekit.TrackID]struct{} // protected by options.tracks.lock
	cleanup          *time.Timer
}

const moqProfile = "livekit-moq-2;moqt=23"

func (m *moqAttachment) issue(endpoint, sid string) *livekit.MoQTransportInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil
	}
	ticket := rand.Text()
	m.generation++
	m.digest = sha256.Sum256([]byte(ticket))
	m.expires = time.Now().Add(30 * time.Second)
	return &livekit.MoQTransportInfo{
		Endpoint: endpoint, ParticipantSid: sid, Generation: m.generation,
		Profile: moqProfile, NamespacePrefix: "livekit", AttachmentToken: ticket, ExpiresAt: m.expires.Unix(),
	}
}

func (m *moqAttachment) attach(generation uint64, ticket string, transport MoQTransport) error {
	digest := sha256.Sum256([]byte(ticket))
	m.mu.Lock()
	if m.closed || generation != m.generation || !time.Now().Before(m.expires) || subtle.ConstantTimeCompare(digest[:], m.digest[:]) != 1 {
		m.mu.Unlock()
		return errors.New("invalid MoQ attachment")
	}
	if m.cleanup != nil {
		m.cleanup.Stop()
		m.cleanup = nil
	}
	previous := m.active
	m.active, m.activeGeneration = transport, generation
	m.digest = [sha256.Size]byte{}
	m.expires = time.Time{}
	m.mu.Unlock()
	if previous != nil {
		previous.Close()
	}
	return nil
}

func (m *moqAttachment) detach(generation uint64, onExpiry func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.active == nil || m.activeGeneration != generation {
		return
	}
	m.active = nil
	m.cleanup = time.AfterFunc(disconnectCleanupDuration, func() {
		m.mu.Lock()
		expired := !m.closed && m.active == nil && m.activeGeneration == generation
		if expired {
			m.closed = true
		}
		m.mu.Unlock()
		if expired {
			onExpiry()
		}
	})
}

func (m *moqAttachment) everConnected() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.activeGeneration != 0
}

func (m *moqAttachment) close() {
	m.mu.Lock()
	m.closed = true
	if m.cleanup != nil {
		m.cleanup.Stop()
		m.cleanup = nil
	}
	previous := m.active
	m.active = nil
	m.mu.Unlock()
	if m.options != nil {
		m.clearSubscriptions()
	}
	if previous != nil {
		previous.Close()
	}
}

// AttachMoQ consumes the single-use ticket sent to this participant on its WebSocket.
func (p *ParticipantImpl) AttachMoQ(generation uint64, ticket string, t MoQTransport) error {
	if !p.UsesMoQ() || p.IsClosed() {
		return errors.New("MoQ participant is unavailable")
	}
	if err := p.moq.attach(generation, ticket, t); err != nil {
		return err
	}
	p.onPrimaryTransportInitialConnected()
	return nil
}

// DetachMoQ starts the resume window only for the session that just disconnected.
func (p *ParticipantImpl) DetachMoQ(generation uint64) {
	if !p.UsesMoQ() {
		return
	}
	p.moq.detach(generation, func() { _ = p.Close(true, types.ParticipantCloseReasonPeerConnectionDisconnected, false) })
}

// ReceiveMoQDataTrack delivers data through the same handle and permission checks as WebRTC.
func (p *ParticipantImpl) ReceiveMoQDataTrack(generation uint64, track types.DataTrack, data []byte, at int64) {
	if !p.IsMoQAttachmentCurrent(generation) || p.IsClosed() || !p.CanPublishData() || p.GetPublishedDataTrack(track.PubHandle()) != track {
		return
	}
	var packet datatrack.Packet
	if err := packet.Unmarshal(data); err != nil || packet.Handle != track.PubHandle() {
		return
	}
	track.HandlePacket(data, &packet, at)
	p.listener().OnDataTrackMessage(p, data, &packet)
}

// IsMoQAttachmentCurrent checks that this media session still owns the participant.
func (p *ParticipantImpl) IsMoQAttachmentCurrent(generation uint64) bool {
	if !p.UsesMoQ() {
		return false
	}
	p.moq.mu.Lock()
	defer p.moq.mu.Unlock()
	return p.moq.active != nil && p.moq.activeGeneration == generation
}

// GetDataTrackTransport returns a stable sink across media reconnections.
func (p *ParticipantImpl) GetDataTrackTransport() types.DataTrackTransport {
	if p.UsesMoQ() {
		return p.moq
	}
	return p.TransportManager.GetDataTrackTransport()
}

func (m *moqAttachment) pathQuality() (float32, livekit.ConnectionQuality) {
	m.mu.Lock()
	transport, wasAttached := m.active, m.activeGeneration != 0
	m.mu.Unlock()
	if transport != nil {
		return transport.PathQuality()
	}
	if wasAttached {
		return connectionquality.MinMOS, livekit.ConnectionQuality_LOST
	}
	return connectionquality.MaxMOS, livekit.ConnectionQuality_EXCELLENT
}

func (m *moqAttachment) SendDataTrackMessage(b []byte) error {
	m.mu.Lock()
	t := m.active
	m.mu.Unlock()
	if t == nil {
		return ErrDataChannelUnavailable
	}
	return t.SendDataTrackMessage(b)
}

// UsesMoQ reports the transport selected at WebSocket join.
func (p *ParticipantImpl) UsesMoQ() bool { return p.moq != nil }

// MoQSubscriptionLimit returns the room's configured limit for this media kind.
func (p *ParticipantImpl) MoQSubscriptionLimit(kind livekit.TrackType) int32 {
	if kind == livekit.TrackType_AUDIO {
		return p.params.SubscriptionLimitAudio
	}
	return p.params.SubscriptionLimitVideo
}

// BindMoQSubscription registers an accepted media tap for room speaker and quality updates.
// The request owns the returned cleanup, including when its QUIC attachment is replaced.
func (p *ParticipantImpl) BindMoQSubscription(generation uint64, publisher livekit.ParticipantID, track types.MediaTrack) func() {
	key := subscribedTrackKey{id: track.ID(), moqGeneration: generation}
	p.SubscriptionManager.markSubscribedTo(publisher, key)
	if !p.Hidden() {
		track.OnTrackSubscribed()
	}
	return func() { p.SubscriptionManager.unmarkSubscribedTo(publisher, key) }
}

// SubscribeToTrack records signaling intent for MoQ consumers.
func (p *ParticipantImpl) SubscribeToTrack(id livekit.TrackID, sync bool) {
	if !p.UsesMoQ() {
		p.SubscriptionManager.SubscribeToTrack(id, sync)
		return
	}
	p.moq.subscribe(id)
}

func (p *ParticipantImpl) UnsubscribeFromTrack(id livekit.TrackID) {
	if !p.UsesMoQ() {
		p.SubscriptionManager.UnsubscribeFromTrack(id)
		return
	}
	m := p.moq
	tracks := m.options.tracks
	tracks.lock.Lock()
	defer tracks.lock.Unlock()
	m.unsubscribeLocked(id)
}

// MoQSubscribedTrackIDs snapshots the participant's signaled media interests.
func (p *ParticipantImpl) MoQSubscribedTrackIDs() []livekit.TrackID {
	if !p.UsesMoQ() {
		return nil
	}
	tracks := p.moq.options.tracks
	tracks.lock.RLock()
	defer tracks.lock.RUnlock()
	return slices.Collect(maps.Keys(p.moq.subscriptions))
}

// WantsMoQTrack reports the current signaling subscription, independent of grants.
func (p *ParticipantImpl) WantsMoQTrack(id livekit.TrackID) bool {
	if !p.UsesMoQ() {
		return false
	}
	tracks := p.moq.options.tracks
	tracks.lock.RLock()
	defer tracks.lock.RUnlock()
	_, ok := p.moq.subscriptions[id]
	return ok
}

type moqSubscribers map[livekit.TrackID]map[*moqAttachment]struct{}

func (subs moqSubscribers) removeTrack(id livekit.TrackID) {
	for subscriber := range subs[id] {
		delete(subscriber.subscriptions, id)
	}
	delete(subs, id)
}

// Keep both indexes under the publication lock so a subscribe racing an
// unpublish cannot leave interest in a nonexistent track.
func (m *moqAttachment) subscribe(id livekit.TrackID) {
	tracks := m.options.tracks
	tracks.lock.Lock()
	defer tracks.lock.Unlock()
	if m.subscriptions == nil || tracks.getTrackInfoLocked(id) == nil {
		return
	}
	if tracks.moqSubscribers == nil {
		tracks.moqSubscribers = make(moqSubscribers)
	}
	if tracks.moqSubscribers[id] == nil {
		tracks.moqSubscribers[id] = make(map[*moqAttachment]struct{})
	}
	tracks.moqSubscribers[id][m] = struct{}{}
	m.subscriptions[id] = struct{}{}
}

func (m *moqAttachment) unsubscribeLocked(id livekit.TrackID) {
	tracks := m.options.tracks
	delete(m.subscriptions, id)
	delete(tracks.moqSubscribers[id], m)
	if len(tracks.moqSubscribers[id]) == 0 {
		delete(tracks.moqSubscribers, id)
	}
}

func (m *moqAttachment) clearSubscriptions() {
	tracks := m.options.tracks
	tracks.lock.Lock()
	defer tracks.lock.Unlock()
	for id := range m.subscriptions {
		m.unsubscribeLocked(id)
	}
	m.subscriptions = nil
}

func (p *ParticipantImpl) handleMoQSignalRequest(req *livekit.SignalRequest) error {
	switch m := req.Message.(type) {
	case *livekit.SignalRequest_Offer, *livekit.SignalRequest_Answer, *livekit.SignalRequest_Trickle:
		return errors.New("WebRTC negotiation on a MoQ participant")
	case *livekit.SignalRequest_UnpublishTrack:
		id := livekit.TrackID(m.UnpublishTrack.GetTrackSid())
		p.pendingTracksLock.Lock()
		p.releasePendingTrackLocked(id)
		t := p.GetPublishedTrack(id)
		p.pendingTracksLock.Unlock()
		if t != nil {
			t.Close(false)
		}
		return nil
	default:
		return p.signalHandler.HandleMessage(req)
	}
}

// StartMoQPublication binds an encoded media receiver to a publication assigned
// by WebSocket AddTrack. Only the active attachment may claim a pending track.
func (p *ParticipantImpl) StartMoQPublication(generation uint64, id livekit.TrackID, codec mime.MimeType) (*media.IngestReceiver, types.MediaTrack, error) {
	if !p.IsMoQAttachmentCurrent(generation) || p.IsClosed() {
		return nil, nil, errors.New("inactive attachment")
	}
	p.pendingTracksLock.Lock()
	for cid, pending := range p.pendingTracks {
		if pending.queued || len(pending.trackInfos) == 0 {
			continue
		}
		ti := utils.CloneProto(pending.trackInfos[0])
		if ti.Sid != string(id) {
			continue
		}
		if !p.CanPublishSource(ti.Source) || p.GetPublishedTrack(id) != nil || ti.Encryption != livekit.Encryption_NONE ||
			(ti.Type == livekit.TrackType_AUDIO && codec != mime.MimeTypeOpus) ||
			(ti.Type == livekit.TrackType_VIDEO && !mime.IsMimeTypeVideo(codec)) {
			break
		}
		enabled := false
		for _, c := range p.params.PublishEnabledCodecs {
			if mime.NormalizeMimeType(c.Mime) == codec {
				enabled = true
				break
			}
		}
		if !enabled {
			break
		}
		ti.MimeType = codec.String()
		receiver, err := media.NewIngestReceiver(media.IngestReceiverParams{
			TrackID: id, StreamID: string(p.ID()), Codec: media.IngestCodec(codec, media.CodecShortName(codec), 2), TrackInfo: ti,
			AudioConfig: p.params.AudioConfig, StreamTrackers: p.params.VideoConfig.StreamTrackerManager,
			PLIThrottle: p.params.PLIThrottleConfig, Logger: p.pubLogger,
		})
		if err != nil {
			p.pendingTracksLock.Unlock()
			return nil, nil, err
		}
		mt := p.addMediaTrack(cid, ti)
		p.pendingTracksLock.Unlock()
		mt.SetPotentialCodecs([]webrtc.RTPCodecParameters{receiver.Codec()}, receiver.HeaderExtensions())
		mt.SetupReceiver(receiver, 0, "")
		p.setIsPublisher(true)
		p.dirty.Store(true)
		p.handleTrackPublished(mt, false, false)
		return receiver, mt, nil
	}
	p.pendingTracksLock.Unlock()
	return nil, nil, errors.New("no permitted signaling publication")
}
