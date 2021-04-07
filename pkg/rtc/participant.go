package rtc

import (
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/ion-sfu/pkg/twcc"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"
	"github.com/pkg/errors"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/logger"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/rtc/types"
	livekit "github.com/livekit/livekit-server/proto"
	"github.com/livekit/livekit-server/version"
	"github.com/livekit/protocol/utils"
)

const (
	placeholderDataChannel = "_private"
	sdBatchSize            = 15
)

type ParticipantImpl struct {
	id           string
	publisher    *PCTransport
	subscriber   *PCTransport
	responseSink routing.MessageSink
	audioConfig  config.AudioConfig
	isClosed     utils.AtomicFlag
	conf         *WebRTCConfig
	identity     string
	permission   *livekit.ParticipantPermission
	state        atomic.Value // livekit.ParticipantInfo_State
	rtcpCh       chan []rtcp.Packet

	// when first connected
	connectedAt atomic.Value // time.Time

	// JSON encoded metadata to pass to clients
	metadata string

	// hold reference for MediaTrack
	twcc *twcc.Responder

	// tracks the current participant is subscribed to, map of otherParticipantId => []DownTrack
	subscribedTracks map[string][]types.SubscribedTrack
	// publishedTracks that participant is publishing
	publishedTracks map[string]types.PublishedTrack
	// client intended to publish, yet to be reconciled
	pendingTracks map[string]*livekit.TrackInfo

	lock sync.RWMutex
	once sync.Once

	// callbacks & handlers
	onTrackPublished func(types.Participant, types.PublishedTrack)
	onTrackUpdated   func(types.Participant, types.PublishedTrack)
	onStateChange    func(p types.Participant, oldState livekit.ParticipantInfo_State)
	onMetadataUpdate func(types.Participant)
	onClose          func(types.Participant)
}

func NewParticipant(identity string, conf *WebRTCConfig, rs routing.MessageSink, ac config.AudioConfig) (*ParticipantImpl, error) {
	// TODO: check to ensure params are valid, id and identity can't be empty

	p := &ParticipantImpl{
		id:               utils.NewGuid(utils.ParticipantPrefix),
		identity:         identity,
		responseSink:     rs,
		audioConfig:      ac,
		conf:             conf,
		rtcpCh:           make(chan []rtcp.Packet, 50),
		subscribedTracks: make(map[string][]types.SubscribedTrack),
		lock:             sync.RWMutex{},
		publishedTracks:  make(map[string]types.PublishedTrack, 0),
		pendingTracks:    make(map[string]*livekit.TrackInfo),
	}
	// store empty value
	p.connectedAt.Store(time.Time{})
	p.state.Store(livekit.ParticipantInfo_JOINING)
	var err error

	p.publisher, err = NewPCTransport(livekit.SignalTarget_PUBLISHER, conf)
	if err != nil {
		return nil, err
	}
	p.subscriber, err = NewPCTransport(livekit.SignalTarget_SUBSCRIBER, conf)
	if err != nil {
		return nil, err
	}

	p.publisher.pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		p.sendIceCandidate(c, livekit.SignalTarget_PUBLISHER)
	})
	p.subscriber.pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		p.sendIceCandidate(c, livekit.SignalTarget_SUBSCRIBER)
	})

	p.publisher.pc.OnICEConnectionStateChange(p.handlePublisherICEStateChange)

	p.publisher.pc.OnTrack(p.onMediaTrack)
	p.subscriber.pc.OnDataChannel(p.onDataChannel)

	p.subscriber.OnNegotiationNeeded(p.negotiate)

	return p, nil
}

func (p *ParticipantImpl) ID() string {
	return p.id
}

func (p *ParticipantImpl) Identity() string {
	return p.identity
}

func (p *ParticipantImpl) State() livekit.ParticipantInfo_State {
	return p.state.Load().(livekit.ParticipantInfo_State)
}

func (p *ParticipantImpl) IsReady() bool {
	state := p.State()
	return state == livekit.ParticipantInfo_JOINED || state == livekit.ParticipantInfo_ACTIVE
}

func (p *ParticipantImpl) ConnectedAt() time.Time {
	return p.connectedAt.Load().(time.Time)
}

// attach metadata to the participant
func (p *ParticipantImpl) SetMetadata(metadata string) {
	p.metadata = metadata

	if p.onMetadataUpdate != nil {
		p.onMetadataUpdate(p)
	}
}

func (p *ParticipantImpl) SetPermission(permission *livekit.ParticipantPermission) {
	p.permission = permission
}

func (p *ParticipantImpl) RTCPChan() chan []rtcp.Packet {
	return p.rtcpCh
}

func (p *ParticipantImpl) ToProto() *livekit.ParticipantInfo {
	info := &livekit.ParticipantInfo{
		Sid:      p.id,
		Identity: p.identity,
		Metadata: p.metadata,
		State:    p.State(),
		JoinedAt: p.ConnectedAt().Unix(),
	}

	p.lock.RLock()
	for _, t := range p.publishedTracks {
		info.Tracks = append(info.Tracks, ToProtoTrack(t))
	}
	p.lock.RUnlock()
	return info
}

func (p *ParticipantImpl) GetResponseSink() routing.MessageSink {
	return p.responseSink
}

func (p *ParticipantImpl) SetResponseSink(sink routing.MessageSink) {
	p.responseSink = sink
}

func (p *ParticipantImpl) SubscriberMediaEngine() *webrtc.MediaEngine {
	return p.subscriber.me
}

// callbacks for clients
func (p *ParticipantImpl) OnTrackPublished(callback func(types.Participant, types.PublishedTrack)) {
	p.onTrackPublished = callback
}

func (p *ParticipantImpl) OnStateChange(callback func(p types.Participant, oldState livekit.ParticipantInfo_State)) {
	p.onStateChange = callback
}

func (p *ParticipantImpl) OnTrackUpdated(callback func(types.Participant, types.PublishedTrack)) {
	p.onTrackUpdated = callback
}

func (p *ParticipantImpl) OnMetadataUpdate(callback func(types.Participant)) {
	p.onMetadataUpdate = callback
}

func (p *ParticipantImpl) OnClose(callback func(types.Participant)) {
	p.onClose = callback
}

// HandleOffer an offer from remote participant, used when clients make the initial connection
func (p *ParticipantImpl) HandleOffer(sdp webrtc.SessionDescription) (answer webrtc.SessionDescription, err error) {
	logger.Debugw("answering pub offer", "state", p.State().String(),
		"participant", p.Identity(),
		//"sdp", sdp.SDP,
	)

	if err = p.publisher.SetRemoteDescription(sdp); err != nil {
		return
	}

	answer, err = p.publisher.pc.CreateAnswer(nil)
	if err != nil {
		err = errors.Wrap(err, "could not create answer")
		return
	}

	if err = p.publisher.pc.SetLocalDescription(answer); err != nil {
		err = errors.Wrap(err, "could not set local description")
		return
	}

	logger.Debugw("sending answer to client",
		"participant", p.Identity(),
		//"sdp", sdp.SDP,
	)
	p.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_Answer{
			Answer: ToProtoSessionDescription(answer),
		},
	})

	if p.State() == livekit.ParticipantInfo_JOINING {
		p.updateState(livekit.ParticipantInfo_JOINED)
	}
	return
}

// client intends to publish track, this function records track details and schedules negotiation
func (p *ParticipantImpl) AddTrack(clientId, name string, trackType livekit.TrackType) {
	p.lock.Lock()
	defer p.lock.Unlock()

	// if track is already published, reject
	if p.pendingTracks[clientId] != nil {
		return
	}

	ti := &livekit.TrackInfo{
		Type: trackType,
		Name: name,
		Sid:  utils.NewGuid(utils.TrackPrefix),
	}
	p.pendingTracks[clientId] = ti

	p.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_TrackPublished{
			TrackPublished: &livekit.TrackPublishedResponse{
				Cid:   clientId,
				Track: ti,
			},
		},
	})
}

func (p *ParticipantImpl) GetPublishedTracks() []types.PublishedTrack {
	p.lock.RLock()
	defer p.lock.RUnlock()
	tracks := make([]types.PublishedTrack, 0, len(p.publishedTracks))
	for _, t := range p.publishedTracks {
		tracks = append(tracks, t)
	}
	return tracks
}

// handles a client answer response, with subscriber PC, server initiates the offer
// and client answers
func (p *ParticipantImpl) HandleAnswer(sdp webrtc.SessionDescription) error {
	if sdp.Type != webrtc.SDPTypeAnswer {
		return ErrUnexpectedOffer
	}
	logger.Debugw("setting subPC answer",
		"participant", p.Identity(),
		//"sdp", sdp.SDP,
	)
	if err := p.subscriber.SetRemoteDescription(sdp); err != nil {
		return errors.Wrap(err, "could not set remote description")
	}

	return nil
}

// AddICECandidate adds candidates for remote peer
func (p *ParticipantImpl) AddICECandidate(candidate webrtc.ICECandidateInit, target livekit.SignalTarget) error {
	var err error
	if target == livekit.SignalTarget_PUBLISHER {
		err = p.publisher.AddICECandidate(candidate)
	} else {
		err = p.subscriber.AddICECandidate(candidate)
	}
	return err
}

func (p *ParticipantImpl) Start() {
	p.once.Do(func() {
		go p.rtcpSendWorker()
		go p.downTracksRTCPWorker()
	})
}

func (p *ParticipantImpl) Close() error {
	if !p.isClosed.TrySet(true) {
		// already closed
		return nil
	}

	// remove all downtracks
	p.lock.Lock()
	for _, t := range p.publishedTracks {
		// skip updates
		t.OnClose(nil)
		t.RemoveAllSubscribers()
	}
	p.lock.Unlock()

	p.updateState(livekit.ParticipantInfo_DISCONNECTED)
	p.subscriber.pc.OnDataChannel(nil)
	p.subscriber.pc.OnICECandidate(nil)
	p.subscriber.pc.OnNegotiationNeeded(nil)
	p.subscriber.pc.OnTrack(nil)
	p.publisher.pc.OnICECandidate(nil)
	// ensure this is synchronized
	p.lock.RLock()
	p.responseSink.Close()
	onClose := p.onClose
	p.lock.RUnlock()
	if onClose != nil {
		onClose(p)
	}
	p.publisher.Close()
	p.subscriber.Close()
	close(p.rtcpCh)
	return nil
}

// Subscribes op to all publishedTracks
func (p *ParticipantImpl) AddSubscriber(op types.Participant) error {
	p.lock.RLock()
	tracks := make([]types.PublishedTrack, 0, len(p.publishedTracks))
	for _, t := range p.publishedTracks {
		tracks = append(tracks, t)
	}
	defer p.lock.RUnlock()

	if len(tracks) == 0 {
		return nil
	}

	logger.Debugw("subscribing new participant to tracks",
		"srcParticipant", p.Identity(),
		"newParticipant", op.Identity(),
		"numTracks", len(tracks))

	for _, track := range tracks {
		if err := track.AddSubscriber(op); err != nil {
			return err
		}
	}
	return nil
}

func (p *ParticipantImpl) RemoveSubscriber(participantId string) {
	p.lock.RLock()
	defer p.lock.RUnlock()

	for _, track := range p.publishedTracks {
		track.RemoveSubscriber(participantId)
	}
}

// signal connection methods
func (p *ParticipantImpl) SendJoinResponse(roomInfo *livekit.Room, otherParticipants []types.Participant, iceServers []*livekit.ICEServer) error {
	// send Join response
	return p.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_Join{
			Join: &livekit.JoinResponse{
				Room:              roomInfo,
				Participant:       p.ToProto(),
				OtherParticipants: ToProtoParticipants(otherParticipants),
				ServerVersion:     version.Version,
				IceServers:        iceServers,
			},
		},
	})
}

func (p *ParticipantImpl) SendParticipantUpdate(participants []*livekit.ParticipantInfo) error {
	if !p.IsReady() {
		return nil
	}

	return p.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_Update{
			Update: &livekit.ParticipantUpdate{
				Participants: participants,
			},
		},
	})
}

func (p *ParticipantImpl) SendActiveSpeakers(speakers []*livekit.SpeakerInfo) error {
	if !p.IsReady() {
		return nil
	}

	return p.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_Speaker{
			Speaker: &livekit.ActiveSpeakerUpdate{
				Speakers: speakers,
			},
		},
	})
}

func (p *ParticipantImpl) SetTrackMuted(trackId string, muted bool) {
	p.lock.RLock()
	defer p.lock.RUnlock()
	track := p.publishedTracks[trackId]
	if track == nil {
		logger.Warnw("could not locate track", "track", trackId)
		return
	}
	currentMuted := track.IsMuted()
	track.SetMuted(muted)

	if currentMuted != track.IsMuted() && p.onTrackUpdated != nil {
		logger.Debugw("mute status changed",
			"participant", p.identity,
			"track", trackId,
			"muted", track.IsMuted())
		p.onTrackUpdated(p, track)
	}
}

func (p *ParticipantImpl) GetAudioLevel() (level uint8, noisy bool) {
	p.lock.RLock()
	defer p.lock.RUnlock()
	level = silentAudioLevel
	for _, pt := range p.publishedTracks {
		if mt, ok := pt.(*MediaTrack); ok {
			if mt.audioLevel == nil {
				continue
			}
			tl, tn := mt.audioLevel.GetLevel()
			if tn {
				noisy = true
				if tl < level {
					level = tl
				}
			}
		}
	}
	return
}

func (p *ParticipantImpl) CanPublish() bool {
	return p.permission == nil || p.permission.CanPublish
}

func (p *ParticipantImpl) CanSubscribe() bool {
	return p.permission == nil || p.permission.CanSubscribe
}

func (p *ParticipantImpl) SubscriberPC() *webrtc.PeerConnection {
	return p.subscriber.pc
}

func (p *ParticipantImpl) GetSubscribedTracks() []types.SubscribedTrack {
	p.lock.RLock()
	defer p.lock.RUnlock()
	subscribed := make([]types.SubscribedTrack, 0, len(p.subscribedTracks))
	for _, pTracks := range p.subscribedTracks {
		for _, t := range pTracks {
			subscribed = append(subscribed, t)
		}
	}
	return subscribed
}

// add a track to the participant's subscribed list
func (p *ParticipantImpl) AddSubscribedTrack(pubId string, subTrack types.SubscribedTrack) {
	logger.Debugw("added subscribedTrack", "srcParticipant", pubId,
		"participant", p.Identity())
	p.lock.Lock()
	p.subscribedTracks[pubId] = append(p.subscribedTracks[pubId], subTrack)
	p.lock.Unlock()
}

// remove a track to the participant's subscribed list
func (p *ParticipantImpl) RemoveSubscribedTrack(pubId string, subTrack types.SubscribedTrack) {
	logger.Debugw("removed subscribedTrack", "srcParticipant", pubId,
		"participant", p.Identity())
	p.lock.Lock()
	defer p.lock.Unlock()
	tracks := make([]types.SubscribedTrack, 0, len(p.subscribedTracks[pubId]))
	for _, tr := range p.subscribedTracks[pubId] {
		if tr != subTrack {
			tracks = append(tracks, tr)
		}
	}
	p.subscribedTracks[pubId] = tracks
}

func (p *ParticipantImpl) sendIceCandidate(c *webrtc.ICECandidate, target livekit.SignalTarget) {
	ci := c.ToJSON()

	// write candidate
	//logger.Debugw("sending ice candidates")
	trickle := ToProtoTrickle(ci)
	trickle.Target = target
	p.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_Trickle{
			Trickle: trickle,
		},
	})
}

// initiates server-driven negotiation by creating an offer
func (p *ParticipantImpl) negotiate() {
	if p.State() == livekit.ParticipantInfo_DISCONNECTED {
		logger.Debugw("skipping server negotiation", "participant", p.Identity())
		// skip when disconnected
		return
	}

	logger.Debugw("starting server negotiation", "participant", p.Identity())

	offer, err := p.subscriber.pc.CreateOffer(nil)
	if err != nil {
		logger.Errorw("could not create offer", "err", err)
		return
	}

	err = p.subscriber.pc.SetLocalDescription(offer)
	if err != nil {
		logger.Errorw("could not set local description", "err", err)
		return
	}

	logger.Debugw("sending offer to participant",
		"participant", p.Identity(),
		//"sdp", offer.SDP,
	)

	p.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_Offer{
			Offer: ToProtoSessionDescription(offer),
		},
	})
}

func (p *ParticipantImpl) updateState(state livekit.ParticipantInfo_State) {
	oldState := p.State()
	if state == oldState {
		return
	}
	p.state.Store(state)
	logger.Debugw("updating participant state", "state", state.String(), "participant", p.Identity())
	if p.onStateChange != nil {
		go func() {
			defer Recover()
			p.onStateChange(p, oldState)
		}()
	}
}

func (p *ParticipantImpl) writeMessage(msg *livekit.SignalResponse) error {
	if p.State() == livekit.ParticipantInfo_DISCONNECTED {
		return nil
	}
	sink := p.responseSink
	err := sink.WriteMessage(msg)
	if err != nil {
		logger.Warnw("could not send message to participant",
			"error", err,
			"id", p.ID(),
			"participant", p.identity,
			"message", fmt.Sprintf("%T", msg.Message))
		return err
	}
	return nil
}

// when a new remoteTrack is created, creates a Track and adds it to room
func (p *ParticipantImpl) onMediaTrack(track *webrtc.TrackRemote, rtpReceiver *webrtc.RTPReceiver) {
	logger.Debugw("mediaTrack added",
		"participant", p.Identity(),
		"remoteTrack", track.ID(),
		"rid", track.RID())

	if !p.CanPublish() {
		logger.Warnw("no permission to publish mediaTrack",
			"participant", p.Identity())
		return
	}

	// delete pending track if it's not simulcasting
	ti := p.getPendingTrack(track.ID(), ToProtoTrackKind(track.Kind()), track.RID() == "")
	if ti == nil {
		return
	}

	// use existing mediatrack to handle simulcast
	p.lock.Lock()
	ptrack := p.publishedTracks[ti.Sid]

	var mt *MediaTrack
	var newTrack bool
	if trk, ok := ptrack.(*MediaTrack); ok {
		mt = trk
	} else {
		mt = NewMediaTrack(ti.Sid, p.id, p.rtcpCh, track, p.conf.BufferFactory, p.conf.Receiver, p.audioConfig)
		mt.name = ti.Name
		newTrack = true
	}

	if p.twcc == nil {
		p.twcc = twcc.NewTransportWideCCResponder(uint32(track.SSRC()))
		p.twcc.OnFeedback(func(pkt rtcp.RawPacket) {
			p.publisher.pc.WriteRTCP([]rtcp.Packet{&pkt})
		})
	}
	mt.AddReceiver(rtpReceiver, track, p.twcc)
	p.lock.Unlock()

	if newTrack {
		p.handleTrackPublished(mt)
	}
}

func (p *ParticipantImpl) onDataChannel(dc *webrtc.DataChannel) {
	if dc.Label() == placeholderDataChannel {
		return
	}
	logger.Debugw("dataChannel added", "participant", p.Identity(), "label", dc.Label())

	if !p.CanPublish() {
		logger.Warnw("no permission to publish dataTrack",
			"participant", p.Identity())
		return
	}

	// data channels have numeric ids, so we use its label to identify
	ti := p.getPendingTrack(dc.Label(), livekit.TrackType_DATA, true)
	if ti == nil {
		return
	}

	dt := NewDataTrack(ti.Sid, p.id, dc)
	dt.name = ti.Name

	p.handleTrackPublished(dt)
}

func (p *ParticipantImpl) getPendingTrack(clientId string, kind livekit.TrackType, deleteAfter bool) *livekit.TrackInfo {
	p.lock.Lock()
	defer p.lock.Unlock()
	ti := p.pendingTracks[clientId]

	// then find the first one that matches type. with MediaStreamTrack, it's possible for the client id to
	// change after being added to SubscriberPC
	if ti == nil {
		for cid, info := range p.pendingTracks {
			if info.Type == kind {
				ti = info
				clientId = cid
				break
			}
		}
	}

	// if still not found, we are done
	if ti == nil {
		logger.Errorw("track info not published prior to track", "clientId", clientId)
	} else if deleteAfter {
		delete(p.pendingTracks, clientId)
	}
	return ti
}

func (p *ParticipantImpl) handleTrackPublished(track types.PublishedTrack) {
	// fill in
	p.lock.Lock()
	p.publishedTracks[track.ID()] = track
	p.lock.Unlock()

	track.Start()

	track.OnClose(func() {
		// cleanup
		p.lock.Lock()
		delete(p.publishedTracks, track.ID())
		p.lock.Unlock()
		// only send this when client is in a ready state
		if p.IsReady() && p.onTrackUpdated != nil {
			p.onTrackUpdated(p, track)
		}
		track.OnClose(nil)
	})

	if p.onTrackPublished != nil {
		p.onTrackPublished(p, track)
	}
}

func (p *ParticipantImpl) handlePublisherICEStateChange(state webrtc.ICEConnectionState) {
	//logger.Debugw("ICE connection state changed", "state", state.String())
	if state == webrtc.ICEConnectionStateConnected {
		p.connectedAt.Store(time.Now())
		p.updateState(livekit.ParticipantInfo_ACTIVE)
	} else if state == webrtc.ICEConnectionStateDisconnected {
		go p.Close()
	}
}

// downTracksRTCPWorker sends SenderReports periodically when the participant is subscribed to
// other publishedTracks in the room.
func (p *ParticipantImpl) downTracksRTCPWorker() {
	defer Recover()
	for {
		time.Sleep(5 * time.Second)

		if p.subscriber.pc.ConnectionState() != webrtc.PeerConnectionStateConnected {
			continue
		}

		var pkts []rtcp.Packet
		var sd []rtcp.SourceDescriptionChunk
		p.lock.RLock()
		for _, tracks := range p.subscribedTracks {
			for _, subTrack := range tracks {
				sr := subTrack.DownTrack().CreateSenderReport()
				chunks := subTrack.DownTrack().CreateSourceDescriptionChunks()
				if sr == nil || chunks == nil {
					continue
				}
				pkts = append(pkts, sr)
				sd = append(sd, chunks...)
			}
		}
		p.lock.RUnlock()

		// now send in batches of sdBatchSize
		// first batch will contain the sender reports too
		var batch []rtcp.SourceDescriptionChunk
		for len(sd) > 0 {
			size := len(sd)
			if size > sdBatchSize {
				size = sdBatchSize
			}
			batch = sd[:size]
			sd = sd[size:]
			pkts = append(pkts, &rtcp.SourceDescription{Chunks: batch})
			if err := p.subscriber.pc.WriteRTCP(pkts); err != nil {
				if err == io.EOF || err == io.ErrClosedPipe {
					return
				}
				logger.Errorw("could not send downtrack reports",
					"participant", p.Identity(),
					"err", err)
			}
			pkts = pkts[:0]
		}
	}
}

func (p *ParticipantImpl) rtcpSendWorker() {
	defer Recover()
	// read from rtcpChan
	for pkts := range p.rtcpCh {
		if pkts == nil {
			return
		}
		//for _, pkt := range pkts {
		//	logger.Debugw("writing RTCP", "packet", pkt)
		//}
		if err := p.publisher.pc.WriteRTCP(pkts); err != nil {
			logger.Errorw("could not write RTCP to participant",
				"participant", p.Identity(),
				"err", err)
		}
	}
}
