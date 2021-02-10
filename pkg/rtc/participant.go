package rtc

import (
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bep/debounce"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"
	"github.com/pkg/errors"
	"github.com/thoas/go-funk"

	"github.com/livekit/livekit-server/pkg/logger"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/utils"
	"github.com/livekit/livekit-server/proto/livekit"
)

const (
	placeholderDataChannel = "_private"
	sdBatchSize            = 20
	negotiationFrequency   = 100 * time.Millisecond
)

const (
	negotiationStateNone = iota
	negotiationStateClient
	negotiationStateServer
)

type ParticipantImpl struct {
	id             string
	peerConn       types.PeerConnection
	responseSink   routing.MessageSink
	receiverConfig ReceiverConfig
	isClosed       utils.AtomicFlag
	mediaEngine    *webrtc.MediaEngine
	identity       string
	state          atomic.Value // livekit.ParticipantInfo_State
	rtcpCh         chan []rtcp.Packet
	// tracks the current participant is subscribed to, map of otherParticipantId => []DownTrack
	subscribedTracks map[string][]types.SubscribedTrack
	// publishedTracks that participant is publishing
	publishedTracks map[string]types.PublishedTrack
	// client intended to publish, yet to be reconciled
	pendingTracks map[string]*livekit.TrackInfo

	negotiationCond    *sync.Cond
	negotiationState   int
	debouncedNegotiate func(func())

	lock sync.RWMutex
	once sync.Once

	// callbacks & handlers
	onTrackPublished func(types.Participant, types.PublishedTrack)
	onTrackUpdated   func(types.Participant, types.PublishedTrack)
	onICECandidate   func(c *webrtc.ICECandidateInit)
	onStateChange    func(p types.Participant, oldState livekit.ParticipantInfo_State)
	onClose          func(types.Participant)
}

func NewPeerConnection(conf *WebRTCConfig) (*webrtc.PeerConnection, error) {
	me, err := createMediaEngine()
	if err != nil {
		return nil, err
	}
	se := conf.SettingEngine
	se.BufferFactory = bufferFactory.GetOrNew

	api := webrtc.NewAPI(webrtc.WithMediaEngine(me), webrtc.WithSettingEngine(se))
	pc, err := api.NewPeerConnection(conf.Configuration)
	return pc, err
}

func NewParticipant(identity string, pc types.PeerConnection, rs routing.MessageSink, receiverConfig ReceiverConfig) (*ParticipantImpl, error) {
	// TODO: check to ensure params are valid, id and identity can't be empty

	participant := &ParticipantImpl{
		id:                 utils.NewGuid(utils.ParticipantPrefix),
		identity:           identity,
		peerConn:           pc,
		responseSink:       rs,
		receiverConfig:     receiverConfig,
		rtcpCh:             make(chan []rtcp.Packet, 50),
		subscribedTracks:   make(map[string][]types.SubscribedTrack),
		lock:               sync.RWMutex{},
		negotiationCond:    sync.NewCond(&sync.Mutex{}),
		publishedTracks:    make(map[string]types.PublishedTrack, 0),
		pendingTracks:      make(map[string]*livekit.TrackInfo),
		debouncedNegotiate: debounce.New(negotiationFrequency),
	}
	participant.state.Store(livekit.ParticipantInfo_JOINING)

	pc.OnTrack(participant.onMediaTrack)

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}

		ci := c.ToJSON()

		// write candidate
		//logger.Debugw("sending ice candidates")
		err := rs.WriteMessage(&livekit.SignalResponse{
			Message: &livekit.SignalResponse_Trickle{
				Trickle: ToProtoTrickle(ci),
			},
		})
		if err != nil {
			logger.Errorw("could not send trickle", "err", err,
				"participant", identity)
		}

		if participant.onICECandidate != nil {
			participant.onICECandidate(&ci)
		}
	})

	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		//logger.Debugw("ICE connection state changed", "state", state.String())
		if state == webrtc.ICEConnectionStateConnected {
			participant.updateState(livekit.ParticipantInfo_ACTIVE)
		} else if state == webrtc.ICEConnectionStateDisconnected {
			go participant.Close()
		}
	})

	pc.OnDataChannel(participant.onDataChannel)

	// only set after answered
	pc.OnNegotiationNeeded(func() {
		logger.Debugw("negotiation needed", "participant", participant.Identity())
		if !participant.IsReady() {
			// ignore negotiation requests before connected
			return
		}
		participant.scheduleNegotiate()
	})

	return participant, nil
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

func (p *ParticipantImpl) RTCPChan() chan []rtcp.Packet {
	return p.rtcpCh
}

func (p *ParticipantImpl) ToProto() *livekit.ParticipantInfo {
	info := &livekit.ParticipantInfo{
		Sid:      p.id,
		Identity: p.identity,
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

// callbacks for clients
func (p *ParticipantImpl) OnTrackPublished(callback func(types.Participant, types.PublishedTrack)) {
	p.onTrackPublished = callback
}

func (p *ParticipantImpl) OnICECandidate(callback func(c *webrtc.ICECandidateInit)) {
	p.onICECandidate = callback
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

// Answer an offer from remote participant, used when clients make the initial connection
func (p *ParticipantImpl) Answer(sdp webrtc.SessionDescription) (answer webrtc.SessionDescription, err error) {
	if p.State() != livekit.ParticipantInfo_JOINING && p.negotiationState != negotiationStateClient {
		// not in a valid state to continue
		err = ErrUnexpectedNegotiation
		return
	}

	logger.Debugw("answering client offer", "state", p.State().String(),
		"participant", p.Identity(),
		//"sdp", sdp.SDP,
	)

	if err = p.peerConn.SetRemoteDescription(sdp); err != nil {
		return
	}

	answer, err = p.peerConn.CreateAnswer(nil)
	if err != nil {
		err = errors.Wrap(err, "could not create answer")
		return
	}

	if err = p.peerConn.SetLocalDescription(answer); err != nil {
		err = errors.Wrap(err, "could not set local description")
		return
	}

	// if this is a client initiated re-negotiation, we'll need to flip back our state
	p.negotiationCond.L.Lock()
	if p.negotiationState == negotiationStateClient {
		p.negotiationState = negotiationStateNone
		p.negotiationCond.Broadcast()
	}
	p.negotiationCond.L.Unlock()

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

func (p *ParticipantImpl) HandleAnswer(sdp webrtc.SessionDescription) error {
	if sdp.Type != webrtc.SDPTypeAnswer {
		return ErrUnexpectedOffer
	}
	logger.Debugw("setting participant answer",
		"participant", p.Identity(),
		//"sdp", sdp.SDP,
	)
	if err := p.peerConn.SetRemoteDescription(sdp); err != nil {
		return errors.Wrap(err, "could not set remote description")
	}

	// negotiated, reset flag
	p.negotiationCond.L.Lock()
	p.negotiationState = negotiationStateNone
	p.negotiationCond.Broadcast()
	p.negotiationCond.L.Unlock()
	return nil
}

// client requested negotiation, when it's able to, send a signal to let it
func (p *ParticipantImpl) HandleClientNegotiation() {
	logger.Debugw("participant requested negotiation",
		"participant", p.Identity())
	// wait until client is able to request negotiation
	p.negotiationCond.L.Lock()
	for p.negotiationState != negotiationStateNone {
		p.negotiationCond.Wait()
	}
	p.negotiationState = negotiationStateClient
	p.negotiationCond.L.Unlock()

	logger.Debugw("allowing participant to negotiate",
		"participant", p.Identity())
	err := p.responseSink.WriteMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_Negotiate{
			Negotiate: &livekit.NegotiationResponse{},
		},
	})
	if err != nil {
		logger.Errorw("could not write message", "error", err,
			"participant", p.identity)
	}
}

// AddICECandidate adds candidates for remote peer
func (p *ParticipantImpl) AddICECandidate(candidate webrtc.ICECandidateInit) error {
	if err := p.peerConn.AddICECandidate(candidate); err != nil {
		return err
	}
	return nil
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
	p.onICECandidate = nil
	p.peerConn.OnDataChannel(nil)
	p.peerConn.OnICECandidate(nil)
	p.peerConn.OnNegotiationNeeded(nil)
	p.peerConn.OnTrack(nil)
	p.responseSink.Close()
	if p.onClose != nil {
		p.onClose(p)
	}
	p.peerConn.Close()
	close(p.rtcpCh)
	return nil
}

// Subscribes otherPeer to all of the publishedTracks
func (p *ParticipantImpl) AddSubscriber(op types.Participant) error {
	p.lock.RLock()
	tracks := funk.Values(p.publishedTracks).([]types.PublishedTrack)
	p.lock.RUnlock()

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

func (p *ParticipantImpl) PeerConnection() types.PeerConnection {
	return p.peerConn
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

func (p *ParticipantImpl) scheduleNegotiate() {
	p.debouncedNegotiate(p.negotiate)
}

// initiates server-driven negotiation by creating an offer
func (p *ParticipantImpl) negotiate() {
	if p.State() == livekit.ParticipantInfo_DISCONNECTED {
		// skip when disconnected
		return
	}

	p.negotiationCond.L.Lock()
	for p.negotiationState != negotiationStateNone {
		p.negotiationCond.Wait()
	}
	p.negotiationState = negotiationStateServer
	p.negotiationCond.L.Unlock()
	logger.Debugw("starting server negotiation", "participant", p.Identity())

	offer, err := p.peerConn.CreateOffer(nil)
	if err != nil {
		logger.Errorw("could not create offer", "err", err)
		return
	}

	if p.peerConn.SignalingState() != webrtc.SignalingStateStable {
		// try this again
		p.scheduleNegotiate()
		return
	}

	err = p.peerConn.SetLocalDescription(offer)
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

	// create ReceiverImpl
	//receiver := NewReceiver(p.rtcpCh, rtpReceiver, track, p.receiverConfig)

	// use existing mediatrack to handle simulcast
	p.lock.RLock()
	ptrack := p.publishedTracks[ti.Sid]
	p.lock.RUnlock()

	var mt *MediaTrack
	var newTrack bool
	if trk, ok := ptrack.(*MediaTrack); ok {
		logger.Debugw("using existing mediatrack, simulcast", "rid", track.RID())
		mt = trk
	} else {
		mt = NewMediaTrack(ti.Sid, p.id, p.rtcpCh, p.receiverConfig, track)
		mt.name = ti.Name
		newTrack = true
	}

	mt.AddReceiver(rtpReceiver, track)

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
	// change after being added to PeerConnection
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
			if err := p.peerConn.WriteRTCP(pkts); err != nil {
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
		if err := p.peerConn.WriteRTCP(pkts); err != nil {
			logger.Errorw("could not write RTCP to participant",
				"participant", p.Identity(),
				"err", err)
		}
	}
}
