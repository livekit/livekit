package rtc

import (
	"encoding/json"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/ion-sfu/pkg/twcc"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"
	"github.com/pkg/errors"
	"github.com/thoas/go-funk"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/logger"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/utils"
	"github.com/livekit/livekit-server/proto/livekit"
	"github.com/livekit/livekit-server/version"
)

const (
	placeholderDataChannel = "_private"
	sdBatchSize            = 20
)

type ParticipantImpl struct {
	id             string
	publisher      *PCTransport
	subscriber     *PCTransport
	responseSink   routing.MessageSink
	receiverConfig ReceiverConfig
	audioConfig    config.AudioConfig
	isClosed       utils.AtomicFlag
	identity       string
	// JSON encoded metadata to pass to clients
	metadata string
	state    atomic.Value // livekit.ParticipantInfo_State
	rtcpCh   chan []rtcp.Packet

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
	onClose          func(types.Participant)
}

func NewParticipant(identity string, conf *WebRTCConfig, rs routing.MessageSink, rc ReceiverConfig, ac config.AudioConfig) (*ParticipantImpl, error) {
	// TODO: check to ensure params are valid, id and identity can't be empty

	p := &ParticipantImpl{
		id:               utils.NewGuid(utils.ParticipantPrefix),
		identity:         identity,
		responseSink:     rs,
		receiverConfig:   rc,
		audioConfig:      ac,
		rtcpCh:           make(chan []rtcp.Packet, 50),
		subscribedTracks: make(map[string][]types.SubscribedTrack),
		lock:             sync.RWMutex{},
		publishedTracks:  make(map[string]types.PublishedTrack, 0),
		pendingTracks:    make(map[string]*livekit.TrackInfo),
	}
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

	p.publisher.pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		//logger.Debugw("ICE connection state changed", "state", state.String())
		if state == webrtc.ICEConnectionStateConnected {
			p.updateState(livekit.ParticipantInfo_ACTIVE)
		} else if state == webrtc.ICEConnectionStateDisconnected {
			go p.Close()
		}
	})

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

// attach metadata to the participant
func (p *ParticipantImpl) SetMetadata(metadata map[string]interface{}) error {
	if metadata == nil {
		p.metadata = ""
		return nil
	}

	if data, err := json.Marshal(metadata); err != nil {
		return err
	} else {
		p.metadata = string(data)
	}

	return nil
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
	err = p.responseSink.WriteMessage(&livekit.SignalResponse{
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

	err := p.responseSink.WriteMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_TrackPublished{
			TrackPublished: &livekit.TrackPublishedResponse{
				Cid:   clientId,
				Track: ti,
			},
		},
	})
	if err != nil {
		logger.Errorw("could not write message", "error", err,
			"participant", p.identity)
	}
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
	p.responseSink.Close()
	if p.onClose != nil {
		p.onClose(p)
	}
	p.publisher.Close()
	p.subscriber.Close()
	close(p.rtcpCh)
	return nil
}

// Subscribes op to all publishedTracks
func (p *ParticipantImpl) AddSubscriber(op types.Participant) error {
	p.lock.RLock()
	tracks := funk.Values(p.publishedTracks).([]types.PublishedTrack)
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
func (p *ParticipantImpl) SendJoinResponse(roomInfo *livekit.Room, otherParticipants []types.Participant) error {
	// send Join response
	return p.responseSink.WriteMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_Join{
			Join: &livekit.JoinResponse{
				Room:              roomInfo,
				Participant:       p.ToProto(),
				OtherParticipants: ToProtoParticipants(otherParticipants),
				ServerVersion:     version.Version,
			},
		},
	})
}

func (p *ParticipantImpl) SendParticipantUpdate(participants []*livekit.ParticipantInfo) error {
	if !p.IsReady() {
		return nil
	}

	return p.responseSink.WriteMessage(&livekit.SignalResponse{
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

	return p.responseSink.WriteMessage(&livekit.SignalResponse{
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

func (p *ParticipantImpl) SubscriberPC() *webrtc.PeerConnection {
	return p.subscriber.pc
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
	err := p.responseSink.WriteMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_Trickle{
			Trickle: trickle,
		},
	})
	if err != nil {
		logger.Errorw("could not send trickle", "err", err,
			"participant", p.identity)
	}
}

// initiates server-driven negotiation by creating an offer
func (p *ParticipantImpl) negotiate() {
	if p.State() == livekit.ParticipantInfo_DISCONNECTED {
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
	err = p.responseSink.WriteMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_Offer{
			Offer: ToProtoSessionDescription(offer),
		},
	})
	if err != nil {
		logger.Errorw("could not send offer to participant",
			"err", err,
			"participant", p.identity)
	}
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

// when a new remoteTrack is created, creates a Track and adds it to room
func (p *ParticipantImpl) onMediaTrack(track *webrtc.TrackRemote, rtpReceiver *webrtc.RTPReceiver) {
	logger.Debugw("mediaTrack added", "participant", p.Identity(), "remoteTrack", track.ID())

	ti := p.popPendingTrack(track.ID(), ToProtoTrackKind(track.Kind()))
	if ti == nil {
		return
	}

	// use existing mediatrack to handle simulcast
	p.lock.Lock()
	ptrack := p.publishedTracks[ti.Sid]

	var mt *MediaTrack
	var newTrack bool
	if trk, ok := ptrack.(*MediaTrack); ok {
		logger.Debugw("using existing mediatrack, simulcast", "rid", track.RID())
		mt = trk
	} else {
		mt = NewMediaTrack(ti.Sid, p.id, p.rtcpCh, track, p.receiverConfig, p.audioConfig)
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

	// data channels have numeric ids, so we use its label to identify
	ti := p.popPendingTrack(dc.Label(), livekit.TrackType_DATA)
	if ti == nil {
		return
	}

	dt := NewDataTrack(ti.Sid, p.id, dc)
	dt.name = ti.Name

	p.handleTrackPublished(dt)
}

func (p *ParticipantImpl) popPendingTrack(clientId string, kind livekit.TrackType) *livekit.TrackInfo {
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
	} else {
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
