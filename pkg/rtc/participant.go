package rtc

import (
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/ion-sfu/pkg/sfu"
	"github.com/pion/ion-sfu/pkg/twcc"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"
	"github.com/pkg/errors"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/protocol/utils"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/logger"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/rtc/types"
	livekit "github.com/livekit/livekit-server/proto"
	"github.com/livekit/livekit-server/version"
)

const (
	lossyDataChannel    = "_lossy"
	reliableDataChannel = "_reliable"
	sdBatchSize         = 20
)

type ParticipantParams struct {
	Identity        string
	Config          *WebRTCConfig
	Sink            routing.MessageSink
	AudioConfig     config.AudioConfig
	ProtocolVersion types.ProtocolVersion
	Stats           *RoomStatsReporter
}

type ParticipantImpl struct {
	params     ParticipantParams
	id         string
	publisher  *PCTransport
	subscriber *PCTransport
	isClosed   utils.AtomicFlag
	permission *livekit.ParticipantPermission
	state      atomic.Value // livekit.ParticipantInfo_State
	rtcpCh     chan []rtcp.Packet

	// reliable and unreliable data channels
	reliableDC *webrtc.DataChannel
	lossyDC    *webrtc.DataChannel

	// when first connected
	connectedAt time.Time

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
	onDataPacket     func(types.Participant, *livekit.DataPacket)
	onClose          func(types.Participant)
}

func NewParticipant(params ParticipantParams) (*ParticipantImpl, error) {
	// TODO: check to ensure params are valid, id and identity can't be empty

	p := &ParticipantImpl{
		params:           params,
		id:               utils.NewGuid(utils.ParticipantPrefix),
		rtcpCh:           make(chan []rtcp.Packet, 50),
		subscribedTracks: make(map[string][]types.SubscribedTrack),
		publishedTracks:  make(map[string]types.PublishedTrack, 0),
		pendingTracks:    make(map[string]*livekit.TrackInfo),
		connectedAt:      time.Now(),
	}
	p.state.Store(livekit.ParticipantInfo_JOINING)

	var err error
	p.publisher, err = NewPCTransport(TransportParams{
		Target: livekit.SignalTarget_PUBLISHER,
		Config: params.Config,
		Stats:  p.params.Stats,
	})
	if err != nil {
		return nil, err
	}
	p.subscriber, err = NewPCTransport(TransportParams{
		Target: livekit.SignalTarget_SUBSCRIBER,
		Config: params.Config,
		Stats:  p.params.Stats,
	})
	if err != nil {
		return nil, err
	}

	p.publisher.pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil || p.State() == livekit.ParticipantInfo_DISCONNECTED {
			return
		}
		p.sendIceCandidate(c, livekit.SignalTarget_PUBLISHER)
	})
	p.subscriber.pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil || p.State() == livekit.ParticipantInfo_DISCONNECTED {
			return
		}
		p.sendIceCandidate(c, livekit.SignalTarget_SUBSCRIBER)
	})

	p.publisher.pc.OnICEConnectionStateChange(p.handlePublisherICEStateChange)
	p.publisher.pc.OnTrack(p.onMediaTrack)
	p.publisher.pc.OnDataChannel(p.onDataChannel)

	p.subscriber.OnOffer(p.onOffer)

	return p, nil
}

func (p *ParticipantImpl) ID() string {
	return p.id
}

func (p *ParticipantImpl) Identity() string {
	return p.params.Identity
}

func (p *ParticipantImpl) State() livekit.ParticipantInfo_State {
	return p.state.Load().(livekit.ParticipantInfo_State)
}

func (p *ParticipantImpl) ProtocolVersion() types.ProtocolVersion {
	return p.params.ProtocolVersion
}

func (p *ParticipantImpl) IsReady() bool {
	state := p.State()
	return state == livekit.ParticipantInfo_JOINED || state == livekit.ParticipantInfo_ACTIVE
}

func (p *ParticipantImpl) ConnectedAt() time.Time {
	return p.connectedAt
}

// SetMetadata attaches metadata to the participant
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
		Identity: p.params.Identity,
		Metadata: p.metadata,
		State:    p.State(),
		JoinedAt: p.ConnectedAt().Unix(),
	}

	p.lock.RLock()
	for _, t := range p.publishedTracks {
		info.Tracks = append(info.Tracks, t.ToProto())
	}
	p.lock.RUnlock()
	return info
}

func (p *ParticipantImpl) GetResponseSink() routing.MessageSink {
	return p.params.Sink
}

func (p *ParticipantImpl) SetResponseSink(sink routing.MessageSink) {
	p.params.Sink = sink
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

func (p *ParticipantImpl) OnDataPacket(callback func(types.Participant, *livekit.DataPacket)) {
	p.onDataPacket = callback
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
	err = p.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_Answer{
			Answer: ToProtoSessionDescription(answer),
		},
	})
	if err != nil {
		return
	}

	if p.State() == livekit.ParticipantInfo_JOINING {
		p.updateState(livekit.ParticipantInfo_JOINED)
	}
	return
}

// AddTrack is called when client intends to publish track.
// records track details and lets client know it's ok to proceed
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

	_ = p.writeMessage(&livekit.SignalResponse{
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

// HandleAnswer handles a client answer response, with subscriber PC, server initiates the
// offer and client answers
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

	// send leave message
	_ = p.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_Leave{
			Leave: &livekit.LeaveRequest{},
		},
	})

	// remove all downtracks
	p.lock.Lock()
	for _, t := range p.publishedTracks {
		// skip updates
		t.OnClose(nil)
		t.RemoveAllSubscribers()
	}

	var downtracksToClose []*sfu.DownTrack
	for _, tracks := range p.subscribedTracks {
		for _, st := range tracks {
			downtracksToClose = append(downtracksToClose, st.DownTrack())
		}
	}
	p.lock.Unlock()

	for _, dt := range downtracksToClose {
		dt.Close()
	}

	p.updateState(livekit.ParticipantInfo_DISCONNECTED)

	// ensure this is synchronized
	p.lock.RLock()
	p.params.Sink.Close()
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

func (p *ParticipantImpl) Negotiate() {
	p.subscriber.Negotiate()
}

// ICERestart restarts subscriber ICE connections
func (p *ParticipantImpl) ICERestart() error {
	if p.subscriber.pc.RemoteDescription() == nil {
		// not connected, skip
		return nil
	}
	return p.subscriber.CreateAndSendOffer(&webrtc.OfferOptions{
		ICERestart: true,
	})
}

// AddSubscriber subscribes op to all publishedTracks
func (p *ParticipantImpl) AddSubscriber(op types.Participant) (int, error) {
	p.lock.RLock()
	tracks := make([]types.PublishedTrack, 0, len(p.publishedTracks))
	for _, t := range p.publishedTracks {
		tracks = append(tracks, t)
	}
	p.lock.RUnlock()

	if len(tracks) == 0 {
		return 0, nil
	}

	logger.Debugw("subscribing new participant to tracks",
		"srcParticipant", p.Identity(),
		"newParticipant", op.Identity(),
		"numTracks", len(tracks))

	n := 0
	for _, track := range tracks {
		if err := track.AddSubscriber(op); err != nil {
			return n, err
		}
		n += 1
	}
	return n, nil
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

func (p *ParticipantImpl) SendDataPacket(dp *livekit.DataPacket) error {
	if p.State() != livekit.ParticipantInfo_ACTIVE {
		return ErrDataChannelUnavailable
	}

	data, err := proto.Marshal(dp)
	if err != nil {
		return err
	}
	if dp.Kind == livekit.DataPacket_RELIABLE {
		if p.reliableDC == nil {
			return ErrDataChannelUnavailable
		}
		return p.reliableDC.Send(data)
	} else {
		if p.lossyDC == nil {
			return ErrDataChannelUnavailable
		}
		return p.lossyDC.Send(data)
	}
}

func (p *ParticipantImpl) SetTrackMuted(trackId string, muted bool) {
	p.lock.RLock()
	defer p.lock.RUnlock()
	track := p.publishedTracks[trackId]
	if track == nil {
		logger.Warnw("could not locate track", nil, "track", trackId)
		return
	}
	currentMuted := track.IsMuted()
	track.SetMuted(muted)

	if currentMuted != track.IsMuted() && p.onTrackUpdated != nil {
		logger.Debugw("mute status changed",
			"participant", p.Identity(),
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

// AddSubscribedTrack adds a track to the participant's subscribed list
func (p *ParticipantImpl) AddSubscribedTrack(pubId string, subTrack types.SubscribedTrack) {
	logger.Debugw("added subscribedTrack", "srcParticipant", pubId,
		"participant", p.Identity(), "track", subTrack.ID())
	p.lock.Lock()
	p.subscribedTracks[pubId] = append(p.subscribedTracks[pubId], subTrack)
	p.lock.Unlock()
}

// RemoveSubscribedTrack removes a track to the participant's subscribed list
func (p *ParticipantImpl) RemoveSubscribedTrack(pubId string, subTrack types.SubscribedTrack) {
	logger.Debugw("removed subscribedTrack", "srcParticipant", pubId,
		"participant", p.Identity(), "track", subTrack.ID())
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
	logger.Debugw("sending ice candidates",
		"participant", p.Identity(),
		"candidate", c.String())
	trickle := ToProtoTrickle(ci)
	trickle.Target = target
	_ = p.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_Trickle{
			Trickle: trickle,
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
	p.lock.RLock()
	onStateChange := p.onStateChange
	p.lock.RUnlock()
	if onStateChange != nil {
		go func() {
			defer Recover()
			onStateChange(p, oldState)
		}()
	}
}

func (p *ParticipantImpl) writeMessage(msg *livekit.SignalResponse) error {
	if p.State() == livekit.ParticipantInfo_DISCONNECTED {
		return nil
	}
	sink := p.params.Sink
	err := sink.WriteMessage(msg)
	if err != nil {
		logger.Warnw("could not send message to participant", err,
			"id", p.ID(),
			"participant", p.Identity(),
			"message", fmt.Sprintf("%T", msg.Message))
		return err
	}
	return nil
}

// when the server has an offer for participant
func (p *ParticipantImpl) onOffer(offer webrtc.SessionDescription) {
	if p.State() == livekit.ParticipantInfo_DISCONNECTED {
		logger.Debugw("skipping server offer", "participant", p.Identity())
		// skip when disconnected
		return
	}

	logger.Debugw("sending server offer to participant",
		"participant", p.Identity(),
		//"sdp", offer.SDP,
	)

	_ = p.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_Offer{
			Offer: ToProtoSessionDescription(offer),
		},
	})
}

// when a new remoteTrack is created, creates a Track and adds it to room
func (p *ParticipantImpl) onMediaTrack(track *webrtc.TrackRemote, rtpReceiver *webrtc.RTPReceiver) {
	if p.State() == livekit.ParticipantInfo_DISCONNECTED {
		return
	}

	logger.Debugw("mediaTrack added",
		"participant", p.Identity(),
		"remoteTrack", track.ID(),
		"rid", track.RID())

	if !p.CanPublish() {
		logger.Warnw("no permission to publish mediaTrack", nil,
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
		mt = NewMediaTrack(track, MediaTrackParams{
			TrackID:        ti.Sid,
			ParticipantID:  p.id,
			RTCPChan:       p.rtcpCh,
			BufferFactory:  p.params.Config.BufferFactory,
			ReceiverConfig: p.params.Config.Receiver,
			AudioConfig:    p.params.AudioConfig,
			Stats:          p.params.Stats,
			Width:          ti.Width,
			Height:         ti.Height,
		})
		mt.name = ti.Name
		newTrack = true
	}

	if p.twcc == nil {
		p.twcc = twcc.NewTransportWideCCResponder(uint32(track.SSRC()))
		p.twcc.OnFeedback(func(pkt rtcp.RawPacket) {
			_ = p.publisher.pc.WriteRTCP([]rtcp.Packet{&pkt})
		})
	}
	mt.AddReceiver(rtpReceiver, track, p.twcc)
	p.lock.Unlock()

	if newTrack {
		p.handleTrackPublished(mt)
	}
}

func (p *ParticipantImpl) onDataChannel(dc *webrtc.DataChannel) {
	if p.State() == livekit.ParticipantInfo_DISCONNECTED {
		return
	}
	switch dc.Label() {
	case reliableDataChannel:
		p.reliableDC = dc
		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			p.handleDataMessage(livekit.DataPacket_RELIABLE, msg.Data)
		})
	case lossyDataChannel:
		p.lossyDC = dc
		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			p.handleDataMessage(livekit.DataPacket_LOSSY, msg.Data)
		})
	default:
		logger.Warnw("unsupported datachannel added", nil, "participant", p.Identity(), "label", dc.Label())
	}
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
		logger.Errorw("track info not published prior to track", nil, "clientId", clientId)
	} else if deleteAfter {
		delete(p.pendingTracks, clientId)
	}
	return ti
}

func (p *ParticipantImpl) handleDataMessage(kind livekit.DataPacket_Kind, data []byte) {
	dp := livekit.DataPacket{}
	if err := proto.Unmarshal(data, &dp); err != nil {
		logger.Warnw("could not parse data packet", err)
		return
	}

	// trust the channel that it came in as the source of truth
	dp.Kind = kind

	// only forward on user payloads
	switch payload := dp.Value.(type) {
	case *livekit.DataPacket_User:
		if p.onDataPacket != nil {
			payload.User.ParticipantSid = p.id
			p.onDataPacket(p, &dp)
		}
	default:
		logger.Warnw("received unsupported data packet", nil, "payload", payload)
	}
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
	// logger.Debugw("ICE connection state changed", "state", state.String(),
	//	"participant", p.identity)
	if state == webrtc.ICEConnectionStateConnected {
		p.updateState(livekit.ParticipantInfo_ACTIVE)
	} else if state == webrtc.ICEConnectionStateDisconnected || state == webrtc.ICEConnectionStateFailed {
		go func() {
			_ = p.Close()
		}()
	}
}

// downTracksRTCPWorker sends SenderReports periodically when the participant is subscribed to
// other publishedTracks in the room.
func (p *ParticipantImpl) downTracksRTCPWorker() {
	defer Recover()
	for {
		time.Sleep(5 * time.Second)

		if p.State() == livekit.ParticipantInfo_DISCONNECTED {
			return
		}
		if p.subscriber.pc.ConnectionState() != webrtc.PeerConnectionStateConnected {
			continue
		}

		var srs []rtcp.Packet
		var sd []rtcp.SourceDescriptionChunk
		p.lock.RLock()
		for _, tracks := range p.subscribedTracks {
			for _, subTrack := range tracks {
				sr := subTrack.DownTrack().CreateSenderReport()
				chunks := subTrack.DownTrack().CreateSourceDescriptionChunks()
				if sr == nil || chunks == nil {
					continue
				}
				srs = append(srs, sr)
				sd = append(sd, chunks...)
			}
		}
		p.lock.RUnlock()

		// now send in batches of sdBatchSize
		var batch []rtcp.SourceDescriptionChunk
		var pkts []rtcp.Packet
		batchSize := 0
		for len(sd) > 0 || len(srs) > 0 {
			numSRs := len(srs)
			if numSRs > 0 {
				if numSRs > sdBatchSize {
					numSRs = sdBatchSize
				}
				pkts = append(pkts, srs[:numSRs]...)
				srs = srs[numSRs:]
			}

			size := len(sd)
			spaceRemain := sdBatchSize - batchSize
			if spaceRemain > 0 && size > 0 {
				if size > spaceRemain {
					size = spaceRemain
				}
				batch = sd[:size]
				sd = sd[size:]
				pkts = append(pkts, &rtcp.SourceDescription{Chunks: batch})
				if err := p.subscriber.pc.WriteRTCP(pkts); err != nil {
					if err == io.EOF || err == io.ErrClosedPipe {
						return
					}
					logger.Errorw("could not send downtrack reports", err,
						"participant", p.Identity())
				}
			}

			pkts = pkts[:0]
			batchSize = 0
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
		// for _, pkt := range pkts {
		//	logger.Debugw("writing RTCP", "packet", pkt)
		// }
		if err := p.publisher.pc.WriteRTCP(pkts); err != nil {
			logger.Errorw("could not write RTCP to participant", err,
				"participant", p.Identity())
		}
	}
}
