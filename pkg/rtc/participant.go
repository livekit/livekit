package rtc

import (
	"context"
	"io"
	"sync"
	"time"

	"github.com/bep/debounce"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"
	"github.com/pkg/errors"

	"github.com/livekit/livekit-server/pkg/logger"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/sfu"
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
	id               string
	peerConn         types.PeerConnection
	responseSink     routing.MessageSink
	receiverConfig   ReceiverConfig
	ctx              context.Context
	cancel           context.CancelFunc
	mediaEngine      *webrtc.MediaEngine
	name             string
	state            livekit.ParticipantInfo_State
	rtcpCh           chan []rtcp.Packet
	subscribedTracks map[string][]*sfu.DownTrack
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
	me := &webrtc.MediaEngine{}
	me.RegisterDefaultCodecs()
	se := conf.SettingEngine
	se.BufferFactory = bufferFactory.GetOrNew

	api := webrtc.NewAPI(webrtc.WithMediaEngine(me), webrtc.WithSettingEngine(se))
	pc, err := api.NewPeerConnection(conf.Configuration)
	return pc, err
}

func NewParticipant(participantId, name string, pc types.PeerConnection, rs routing.MessageSink, receiverConfig ReceiverConfig) (*ParticipantImpl, error) {
	// TODO: check to ensure params are valid, id and name can't be empty
	me := &webrtc.MediaEngine{}
	me.RegisterDefaultCodecs()

	ctx, cancel := context.WithCancel(context.Background())
	participant := &ParticipantImpl{
		id:                 participantId,
		name:               name,
		peerConn:           pc,
		responseSink:       rs,
		receiverConfig:     receiverConfig,
		ctx:                ctx,
		cancel:             cancel,
		rtcpCh:             make(chan []rtcp.Packet, 50),
		subscribedTracks:   make(map[string][]*sfu.DownTrack),
		state:              livekit.ParticipantInfo_JOINING,
		lock:               sync.RWMutex{},
		negotiationCond:    sync.NewCond(&sync.Mutex{}),
		publishedTracks:    make(map[string]types.PublishedTrack, 0),
		pendingTracks:      make(map[string]*livekit.TrackInfo),
		mediaEngine:        me,
		debouncedNegotiate: debounce.New(negotiationFrequency),
	}

	pc.OnTrack(participant.onMediaTrack)

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}

		ci := c.ToJSON()

		// write candidate
		logger.Debugw("sending ice candidates")
		err := rs.WriteMessage(&livekit.SignalResponse{
			Message: &livekit.SignalResponse_Trickle{
				Trickle: ToProtoTrickle(ci),
			},
		})
		if err != nil {
			logger.Errorw("could not send trickle", "err", err)
		}

		if participant.onICECandidate != nil {
			participant.onICECandidate(&ci)
		}
	})

	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		logger.Debugw("ICE connection state changed", "state", state.String())
		if state == webrtc.ICEConnectionStateConnected {
			participant.updateState(livekit.ParticipantInfo_ACTIVE)
		} else if state == webrtc.ICEConnectionStateDisconnected {
			go participant.Close()
		}
	})

	pc.OnDataChannel(participant.onDataChannel)

	// only set after answered
	pc.OnNegotiationNeeded(func() {
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

func (p *ParticipantImpl) Name() string {
	return p.name
}

func (p *ParticipantImpl) State() livekit.ParticipantInfo_State {
	return p.state
}

func (p *ParticipantImpl) IsReady() bool {
	return p.state == livekit.ParticipantInfo_JOINED || p.state == livekit.ParticipantInfo_ACTIVE
}

func (p *ParticipantImpl) RTCPChan() chan<- []rtcp.Packet {
	return p.rtcpCh
}

func (p *ParticipantImpl) ToProto() *livekit.ParticipantInfo {
	info := &livekit.ParticipantInfo{
		Sid:   p.id,
		Name:  p.name,
		State: p.state,
	}

	p.lock.RLock()
	for _, t := range p.publishedTracks {
		info.Tracks = append(info.Tracks, ToProtoTrack(t))
	}
	p.lock.RUnlock()
	return info
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
	if p.state != livekit.ParticipantInfo_JOINING && p.negotiationState != negotiationStateClient {
		// not in a valid state to continue
		err = ErrUnexpectedNegotiation
		return
	}

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

	err = p.responseSink.WriteMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_Answer{
			Answer: ToProtoSessionDescription(answer),
		},
	})

	if p.state == livekit.ParticipantInfo_JOINING {
		p.updateState(livekit.ParticipantInfo_JOINED)
	}
	return
}

// client intends to publish track, this function records track details and schedules negotiation
func (p *ParticipantImpl) AddTrack(clientId, name string, trackType livekit.TrackType) {
	p.lock.Lock()
	defer p.lock.Unlock()

	ti := &livekit.TrackInfo{
		Type: trackType,
		Name: name,
		Sid:  utils.NewGuid(utils.TrackPrefix),
	}
	p.pendingTracks[clientId] = ti

	p.responseSink.WriteMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_TrackPublished{
			TrackPublished: &livekit.TrackPublishedResponse{
				Cid:   clientId,
				Track: ti,
			},
		},
	})
}

func (p *ParticipantImpl) HandleAnswer(sdp webrtc.SessionDescription) error {
	if sdp.Type != webrtc.SDPTypeAnswer {
		return ErrUnexpectedOffer
	}
	logger.Debugw("setting remote answer")
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
	// wait until client is able to request negotiation
	p.negotiationCond.L.Lock()
	for p.negotiationState != negotiationStateNone {
		p.negotiationCond.Wait()
	}
	p.negotiationState = negotiationStateClient
	p.negotiationCond.L.Unlock()
	p.responseSink.WriteMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_Negotiate{
			Negotiate: &livekit.NegotiationResponse{},
		},
	})
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
	if p.ctx.Err() != nil {
		return p.ctx.Err()
	}
	p.onICECandidate = nil
	p.peerConn.OnDataChannel(nil)
	p.peerConn.OnICECandidate(nil)
	p.peerConn.OnNegotiationNeeded(nil)
	p.peerConn.OnTrack(nil)
	p.updateState(livekit.ParticipantInfo_DISCONNECTED)
	p.responseSink.Close()
	if p.onClose != nil {
		p.onClose(p)
	}
	p.cancel()
	close(p.rtcpCh)
	return p.peerConn.Close()
}

// Subscribes otherPeer to all of the publishedTracks
func (p *ParticipantImpl) AddSubscriber(op types.Participant) error {
	p.lock.RLock()
	defer p.lock.RUnlock()

	for _, track := range p.publishedTracks {
		logger.Debugw("subscribing to remoteTrack",
			"srcParticipant", p.ID(),
			"dstParticipant", op.ID(),
			"remoteTrack", track.ID())
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
		return
	}
	updated := false

	switch t := track.(type) {
	case *MediaTrack:
		updated = t.muted != muted
		t.muted = muted
	}

	if updated && p.onTrackUpdated != nil {
		p.onTrackUpdated(p, track)
	}
}

func (p *ParticipantImpl) PeerConnection() types.PeerConnection {
	return p.peerConn
}

func (p *ParticipantImpl) AddDownTrack(streamId string, dt *sfu.DownTrack) {
	p.lock.Lock()
	p.subscribedTracks[streamId] = append(p.subscribedTracks[streamId], dt)
	p.lock.Unlock()
}

func (p *ParticipantImpl) RemoveDownTrack(streamId string, dt *sfu.DownTrack) {
	p.lock.Lock()
	defer p.lock.Unlock()
	tracks := p.subscribedTracks[streamId]
	newTracks := make([]*sfu.DownTrack, 0, len(tracks))
	for _, track := range tracks {
		if track != dt {
			newTracks = append(newTracks, track)
		}
	}
	p.subscribedTracks[streamId] = newTracks
}

func (p *ParticipantImpl) scheduleNegotiate() {
	p.debouncedNegotiate(p.negotiate)
}

// initiates server-driven negotiation by creating an offer
func (p *ParticipantImpl) negotiate() {
	if p.state == livekit.ParticipantInfo_DISCONNECTED {
		// skip when disconnected
		return
	}
	p.negotiationCond.L.Lock()
	for p.negotiationState != negotiationStateNone {
		p.negotiationCond.Wait()
		p.negotiationState = negotiationStateServer
	}
	p.negotiationCond.L.Unlock()

	logger.Debugw("starting negotiation", "participant", p.ID())
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

	logger.Debugw("sending available offer to participant")
	err = p.responseSink.WriteMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_Offer{
			Offer: ToProtoSessionDescription(offer),
		},
	})
	if err != nil {
		logger.Errorw("could not send offer to peer",
			"err", err)
	}
}

func (p *ParticipantImpl) updateState(state livekit.ParticipantInfo_State) {
	if state == p.state {
		return
	}
	oldState := p.state
	p.state = state
	logger.Debugw("updating participant state", "state", state.String())
	if p.onStateChange != nil {
		go func() {
			defer Recover()
			p.onStateChange(p, oldState)
		}()
	}
}

// when a new remoteTrack is created, creates a Track and adds it to room
func (p *ParticipantImpl) onMediaTrack(track *webrtc.TrackRemote, rtpReceiver *webrtc.RTPReceiver) {
	logger.Debugw("mediaTrack added", "participantId", p.ID(), "remoteTrack", track.ID())

	ti := p.popPendingTrack(track.ID())
	if ti == nil {
		return
	}

	// create ReceiverImpl
	receiver := NewReceiver(p.rtcpCh, rtpReceiver, track, p.receiverConfig)
	mt := NewMediaTrack(ti.Sid, p.id, p.rtcpCh, track, receiver)
	mt.name = ti.Name

	p.handleTrackPublished(mt)
}

func (p *ParticipantImpl) onDataChannel(dc *webrtc.DataChannel) {
	if dc.Label() == placeholderDataChannel {
		return
	}
	logger.Debugw("dataChannel added", "participantId", p.ID(), "label", dc.Label())

	// data channels have numeric ids, so we use its label to identify
	ti := p.popPendingTrack(dc.Label())
	if ti == nil {
		return
	}

	dt := NewDataTrack(ti.Sid, p.id, dc)
	dt.name = ti.Name

	p.handleTrackPublished(dt)
}

func (p *ParticipantImpl) popPendingTrack(clientId string) *livekit.TrackInfo {
	p.lock.Lock()
	defer p.lock.Unlock()
	ti := p.pendingTracks[clientId]
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
		if p.onTrackUpdated != nil {
			p.onTrackUpdated(p, track)
		}
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
		for _, dts := range p.subscribedTracks {
			for _, dt := range dts {
				if !dt.IsBound() {
					continue
				}
				pkts = append(pkts, dt.CreateSenderReport())
				chunks := dt.CreateSourceDescriptionChunks()
				if chunks != nil {
					sd = append(sd, chunks...)
				}
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
					"participant", p.id,
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
		//for _, pkt := range pkts {
		//	logger.Debugw("writing RTCP", "packet", pkt)
		//}
		if err := p.peerConn.WriteRTCP(pkts); err != nil {
			logger.Errorw("could not write RTCP to participant",
				"participant", p.id,
				"err", err)
		}
	}
}
