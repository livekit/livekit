package rtc

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/livekit/livekit-server/pkg/sfu/connectionquality"

	lru "github.com/hashicorp/golang-lru"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"
	"github.com/pkg/errors"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/sfu"
	"github.com/livekit/livekit-server/pkg/sfu/twcc"
	"github.com/livekit/livekit-server/pkg/telemetry"
	"github.com/livekit/livekit-server/pkg/telemetry/prometheus"
	"github.com/livekit/livekit-server/version"
)

const (
	lossyDataChannel    = "_lossy"
	reliableDataChannel = "_reliable"
	sdBatchSize         = 20
)

type ParticipantParams struct {
	Identity                livekit.ParticipantIdentity
	Name                    livekit.ParticipantName
	SID                     livekit.ParticipantID
	Config                  *WebRTCConfig
	Sink                    routing.MessageSink
	AudioConfig             config.AudioConfig
	ProtocolVersion         types.ProtocolVersion
	Telemetry               telemetry.TelemetryService
	ThrottleConfig          config.PLIThrottleConfig
	CongestionControlConfig config.CongestionControlConfig
	EnabledCodecs           []*livekit.Codec
	Hidden                  bool
	Recorder                bool
	Logger                  logger.Logger
}

type ParticipantImpl struct {
	params      ParticipantParams
	publisher   *PCTransport
	subscriber  *PCTransport
	isClosed    utils.AtomicFlag
	permission  *livekit.ParticipantPermission
	state       atomic.Value // livekit.ParticipantInfo_State
	updateCache *lru.Cache

	// reliable and unreliable data channels
	reliableDC    *webrtc.DataChannel
	reliableDCSub *webrtc.DataChannel
	lossyDC       *webrtc.DataChannel
	lossyDCSub    *webrtc.DataChannel

	// when first connected
	connectedAt time.Time

	// JSON encoded metadata to pass to clients
	metadata string

	// hold reference for MediaTrack
	twcc *twcc.Responder

	uptrackManager *UptrackManager

	// tracks the current participant is subscribed to, map of sid => DownTrack
	subscribedTracks map[livekit.TrackID]types.SubscribedTrack
	// keeps track of disallowed tracks
	disallowedSubscriptions map[livekit.TrackID]livekit.ParticipantID // trackID -> publisherID
	// keep track of other publishers identities that we are subscribed to
	subscribedTo sync.Map // livekit.ParticipantID => struct{}

	lock       sync.RWMutex
	once       sync.Once
	updateLock sync.Mutex

	// callbacks & handlers
	onTrackPublished func(types.Participant, types.PublishedTrack)
	onTrackUpdated   func(types.Participant, types.PublishedTrack)
	onStateChange    func(p types.Participant, oldState livekit.ParticipantInfo_State)
	onMetadataUpdate func(types.Participant)
	onDataPacket     func(types.Participant, *livekit.DataPacket)
	onClose          func(types.Participant, map[livekit.TrackID]livekit.ParticipantID)
}

func NewParticipant(params ParticipantParams) (*ParticipantImpl, error) {
	// TODO: check to ensure params are valid, id and identity can't be empty

	p := &ParticipantImpl{
		params:                  params,
		subscribedTracks:        make(map[livekit.TrackID]types.SubscribedTrack),
		disallowedSubscriptions: make(map[livekit.TrackID]livekit.ParticipantID),
		connectedAt:             time.Now(),
	}
	p.state.Store(livekit.ParticipantInfo_JOINING)

	var err error
	// keep last participants and when updates were sent
	if p.updateCache, err = lru.New(32); err != nil {
		return nil, err
	}
	p.publisher, err = NewPCTransport(TransportParams{
		ParticipantID:           p.params.SID,
		ParticipantIdentity:     p.params.Identity,
		Target:                  livekit.SignalTarget_PUBLISHER,
		Config:                  params.Config,
		CongestionControlConfig: params.CongestionControlConfig,
		Telemetry:               p.params.Telemetry,
		EnabledCodecs:           p.params.EnabledCodecs,
		Logger:                  params.Logger,
	})
	if err != nil {
		return nil, err
	}
	p.subscriber, err = NewPCTransport(TransportParams{
		ParticipantID:           p.params.SID,
		ParticipantIdentity:     p.params.Identity,
		Target:                  livekit.SignalTarget_SUBSCRIBER,
		Config:                  params.Config,
		CongestionControlConfig: params.CongestionControlConfig,
		Telemetry:               p.params.Telemetry,
		EnabledCodecs:           p.params.EnabledCodecs,
		Logger:                  params.Logger,
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

	primaryPC := p.publisher.pc

	if p.SubscriberAsPrimary() {
		primaryPC = p.subscriber.pc
		ordered := true
		// also create data channels for subs
		p.reliableDCSub, err = primaryPC.CreateDataChannel(reliableDataChannel, &webrtc.DataChannelInit{
			Ordered: &ordered,
		})
		if err != nil {
			return nil, err
		}
		retransmits := uint16(0)
		p.lossyDCSub, err = primaryPC.CreateDataChannel(lossyDataChannel, &webrtc.DataChannelInit{
			Ordered:        &ordered,
			MaxRetransmits: &retransmits,
		})
		if err != nil {
			return nil, err
		}
	}
	primaryPC.OnICEConnectionStateChange(p.handlePrimaryICEStateChange)
	p.publisher.pc.OnTrack(p.onMediaTrack)
	p.publisher.pc.OnDataChannel(p.onDataChannel)

	p.subscriber.OnOffer(p.onOffer)

	p.subscriber.OnStreamStateChange(p.onStreamStateChange)

	p.setupUptrackManager()

	return p, nil
}

func (p *ParticipantImpl) ID() livekit.ParticipantID {
	return p.params.SID
}

func (p *ParticipantImpl) Identity() livekit.ParticipantIdentity {
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

func (p *ParticipantImpl) ToProto() *livekit.ParticipantInfo {
	info := &livekit.ParticipantInfo{
		Sid:      string(p.params.SID),
		Identity: string(p.params.Identity),
		Name:     string(p.params.Name),
		Metadata: p.metadata,
		State:    p.State(),
		JoinedAt: p.ConnectedAt().Unix(),
		Hidden:   p.Hidden(),
		Recorder: p.IsRecorder(),
	}
	info.Tracks = p.uptrackManager.ToProto()

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

func (p *ParticipantImpl) OnClose(callback func(types.Participant, map[livekit.TrackID]livekit.ParticipantID)) {
	p.onClose = callback
}

// HandleOffer an offer from remote participant, used when clients make the initial connection
func (p *ParticipantImpl) HandleOffer(sdp webrtc.SessionDescription) (answer webrtc.SessionDescription, err error) {
	p.params.Logger.Debugw("answering pub offer",
		"state", p.State().String(),
		// "sdp", sdp.SDP,
	)

	if err = p.publisher.SetRemoteDescription(sdp); err != nil {
		prometheus.ServiceOperationCounter.WithLabelValues("answer", "error", "remote_description").Add(1)
		return
	}

	p.configureReceiverDTX()

	answer, err = p.publisher.pc.CreateAnswer(nil)
	if err != nil {
		prometheus.ServiceOperationCounter.WithLabelValues("answer", "error", "create").Add(1)
		err = errors.Wrap(err, "could not create answer")
		return
	}

	if err = p.publisher.pc.SetLocalDescription(answer); err != nil {
		prometheus.ServiceOperationCounter.WithLabelValues("answer", "error", "local_description").Add(1)
		err = errors.Wrap(err, "could not set local description")
		return
	}

	p.params.Logger.Debugw("sending answer to client")

	err = p.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_Answer{
			Answer: ToProtoSessionDescription(answer),
		},
	})
	if err != nil {
		prometheus.ServiceOperationCounter.WithLabelValues("answer", "error", "write_message").Add(1)
		return
	}

	if p.State() == livekit.ParticipantInfo_JOINING {
		p.updateState(livekit.ParticipantInfo_JOINED)
	}
	prometheus.ServiceOperationCounter.WithLabelValues("answer", "success", "").Add(1)

	return
}

// AddTrack is called when client intends to publish track.
// records track details and lets client know it's ok to proceed
func (p *ParticipantImpl) AddTrack(req *livekit.AddTrackRequest) {
	p.lock.Lock()
	defer p.lock.Unlock()

	if !p.CanPublish() {
		p.params.Logger.Warnw("no permission to publish track", nil)
		return
	}

	ti := p.uptrackManager.AddTrack(req)
	if ti == nil {
		return
	}

	_ = p.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_TrackPublished{
			TrackPublished: &livekit.TrackPublishedResponse{
				Cid:   req.Cid,
				Track: ti,
			},
		},
	})
}

// HandleAnswer handles a client answer response, with subscriber PC, server initiates the
// offer and client answers
func (p *ParticipantImpl) HandleAnswer(sdp webrtc.SessionDescription) error {
	if sdp.Type != webrtc.SDPTypeAnswer {
		return ErrUnexpectedOffer
	}
	p.params.Logger.Debugw("setting subPC answer")

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
		p.uptrackManager.Start()
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

	p.uptrackManager.Close()

	p.lock.Lock()
	disallowedSubscriptions := make(map[livekit.TrackID]livekit.ParticipantID)
	for trackID, publisherID := range p.disallowedSubscriptions {
		disallowedSubscriptions[trackID] = publisherID
	}

	// remove all downtracks
	var downtracksToClose []*sfu.DownTrack
	for _, st := range p.subscribedTracks {
		downtracksToClose = append(downtracksToClose, st.DownTrack())
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
		onClose(p, disallowedSubscriptions)
	}
	p.publisher.Close()
	p.subscriber.Close()
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

// AddSubscriber subscribes op to all publishedTracks or given set of tracks
func (p *ParticipantImpl) AddSubscriber(op types.Participant, params types.AddSubscriberParams) (int, error) {
	return p.uptrackManager.AddSubscriber(op, params)
}

func (p *ParticipantImpl) RemoveSubscriber(op types.Participant, trackID livekit.TrackID) {
	p.uptrackManager.RemoveSubscriber(op, trackID)
}

// signal connection methods

func (p *ParticipantImpl) SendJoinResponse(
	roomInfo *livekit.Room,
	otherParticipants []*livekit.ParticipantInfo,
	iceServers []*livekit.ICEServer,
) error {
	// send Join response
	return p.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_Join{
			Join: &livekit.JoinResponse{
				Room:              roomInfo,
				Participant:       p.ToProto(),
				OtherParticipants: otherParticipants,
				ServerVersion:     version.Version,
				IceServers:        iceServers,
				// indicates both server and client support subscriber as primary
				SubscriberPrimary: p.SubscriberAsPrimary(),
			},
		},
	})
}

func (p *ParticipantImpl) SendParticipantUpdate(participantsToUpdate []*livekit.ParticipantInfo, updatedAt time.Time) error {
	if len(participantsToUpdate) == 1 {
		p.updateLock.Lock()
		defer p.updateLock.Unlock()
		pi := participantsToUpdate[0]
		if val, ok := p.updateCache.Get(pi.Sid); ok {
			if lastUpdatedAt, ok := val.(time.Time); ok {
				// this is a message delivered out of order, a more recent version of the message had already been
				// sent.
				if lastUpdatedAt.After(updatedAt) {
					return nil
				}
			}
		}
		p.updateCache.Add(pi.Sid, updatedAt)
	}
	return p.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_Update{
			Update: &livekit.ParticipantUpdate{
				Participants: participantsToUpdate,
			},
		},
	})
}

// SendSpeakerUpdate notifies participant changes to speakers. only send members that have changed since last update
func (p *ParticipantImpl) SendSpeakerUpdate(speakers []*livekit.SpeakerInfo) error {
	if !p.IsReady() {
		return nil
	}

	var scopedSpeakers []*livekit.SpeakerInfo
	for _, s := range speakers {
		participantID := livekit.ParticipantID(s.Sid)
		if p.IsSubscribedTo(participantID) || participantID == p.ID() {
			scopedSpeakers = append(scopedSpeakers, s)
		}
	}

	if len(scopedSpeakers) == 0 {
		return nil
	}

	return p.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_SpeakersChanged{
			SpeakersChanged: &livekit.SpeakersChanged{
				Speakers: scopedSpeakers,
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

	var dc *webrtc.DataChannel
	if dp.Kind == livekit.DataPacket_RELIABLE {
		if p.SubscriberAsPrimary() {
			dc = p.reliableDCSub
		} else {
			dc = p.reliableDC
		}
	} else {
		if p.SubscriberAsPrimary() {
			dc = p.lossyDCSub
		} else {
			dc = p.lossyDC
		}
	}

	if dc == nil {
		return ErrDataChannelUnavailable
	}
	return dc.Send(data)
}

func (p *ParticipantImpl) SendRoomUpdate(room *livekit.Room) error {
	return p.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_RoomUpdate{
			RoomUpdate: &livekit.RoomUpdate{
				Room: room,
			},
		},
	})
}

func (p *ParticipantImpl) SendConnectionQualityUpdate(update *livekit.ConnectionQualityUpdate) error {
	return p.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_ConnectionQuality{
			ConnectionQuality: update,
		},
	})
}

func (p *ParticipantImpl) SetTrackMuted(trackID livekit.TrackID, muted bool, fromAdmin bool) {
	// when request is coming from admin, send message to current participant
	if fromAdmin {
		_ = p.writeMessage(&livekit.SignalResponse{
			Message: &livekit.SignalResponse_Mute{
				Mute: &livekit.MuteTrackRequest{
					Sid:   string(trackID),
					Muted: muted,
				},
			},
		})
	}

	p.uptrackManager.SetTrackMuted(trackID, muted)
}

func (p *ParticipantImpl) GetAudioLevel() (level uint8, active bool) {
	return p.uptrackManager.GetAudioLevel()
}

func (p *ParticipantImpl) GetConnectionQuality() *livekit.ConnectionQualityInfo {
	// avg loss across all tracks, weigh published the same as subscribed
	scores, numTracks := p.uptrackManager.GetConnectionQuality()

	p.lock.RLock()
	for _, subTrack := range p.subscribedTracks {
		if subTrack.IsMuted() || subTrack.MediaTrack().IsMuted() {
			continue
		}
		scores += subTrack.DownTrack().GetConnectionScore()
		numTracks++
	}
	p.lock.RUnlock()

	avgScore := 5.0
	if numTracks > 0 {
		avgScore = scores / float64(numTracks)
	}

	rating := connectionquality.Score2Rating(avgScore)

	return &livekit.ConnectionQualityInfo{
		ParticipantSid: string(p.ID()),
		Quality:        rating,
		Score:          float32(avgScore),
	}
}

func (p *ParticipantImpl) IsSubscribedTo(participantID livekit.ParticipantID) bool {
	_, ok := p.subscribedTo.Load(participantID)
	return ok
}

func (p *ParticipantImpl) GetSubscribedParticipants() []livekit.ParticipantID {
	var participantIDs []livekit.ParticipantID
	p.subscribedTo.Range(func(key, _ interface{}) bool {
		if participantID, ok := key.(livekit.ParticipantID); ok {
			participantIDs = append(participantIDs, participantID)
		}
		return true
	})
	return participantIDs
}

func (p *ParticipantImpl) CanPublish() bool {
	return p.permission == nil || p.permission.CanPublish
}

func (p *ParticipantImpl) CanSubscribe() bool {
	return p.permission == nil || p.permission.CanSubscribe
}

func (p *ParticipantImpl) CanPublishData() bool {
	return p.permission == nil || p.permission.CanPublishData
}

func (p *ParticipantImpl) Hidden() bool {
	return p.params.Hidden
}

func (p *ParticipantImpl) IsRecorder() bool {
	return p.params.Recorder
}

func (p *ParticipantImpl) SubscriberAsPrimary() bool {
	return p.ProtocolVersion().SubscriberAsPrimary() && p.CanSubscribe()
}

func (p *ParticipantImpl) SubscriberPC() *webrtc.PeerConnection {
	return p.subscriber.pc
}

func (p *ParticipantImpl) GetPublishedTrack(sid livekit.TrackID) types.PublishedTrack {
	return p.uptrackManager.GetPublishedTrack(sid)
}

func (p *ParticipantImpl) GetPublishedTracks() []types.PublishedTrack {
	return p.uptrackManager.GetPublishedTracks()
}

func (p *ParticipantImpl) GetSubscribedTrack(sid livekit.TrackID) types.SubscribedTrack {
	p.lock.RLock()
	defer p.lock.RUnlock()
	return p.subscribedTracks[sid]
}

func (p *ParticipantImpl) GetSubscribedTracks() []types.SubscribedTrack {
	p.lock.RLock()
	defer p.lock.RUnlock()
	subscribed := make([]types.SubscribedTrack, 0, len(p.subscribedTracks))
	for _, st := range p.subscribedTracks {
		subscribed = append(subscribed, st)
	}
	return subscribed
}

// AddSubscribedTrack adds a track to the participant's subscribed list
func (p *ParticipantImpl) AddSubscribedTrack(subTrack types.SubscribedTrack) {
	p.params.Logger.Debugw("added subscribedTrack",
		"publisherID", subTrack.PublisherID(),
		"publisherIdentity", subTrack.PublisherIdentity(),
		"track", subTrack.ID())
	p.lock.Lock()
	p.subscribedTracks[subTrack.ID()] = subTrack
	p.lock.Unlock()

	subTrack.OnBind(func() {
		p.subscriber.AddTrack(subTrack)
	})
	p.subscribedTo.Store(subTrack.PublisherID(), struct{}{})
}

// RemoveSubscribedTrack removes a track to the participant's subscribed list
func (p *ParticipantImpl) RemoveSubscribedTrack(subTrack types.SubscribedTrack) {
	p.params.Logger.Debugw("removed subscribedTrack",
		"publisherID", subTrack.PublisherID(),
		"publisherIdentity", subTrack.PublisherIdentity(),
		"track", subTrack.ID(), "kind", subTrack.DownTrack().Kind())

	p.subscriber.RemoveTrack(subTrack)

	p.lock.Lock()
	delete(p.subscribedTracks, subTrack.ID())
	// remove from subscribed map
	numRemaining := 0
	for _, st := range p.subscribedTracks {
		if st.PublisherID() == subTrack.PublisherID() {
			numRemaining++
		}
	}
	p.lock.Unlock()
	if numRemaining == 0 {
		p.subscribedTo.Delete(subTrack.PublisherID())
	}
}

func (p *ParticipantImpl) UpdateSubscriptionPermissions(
	permissions *livekit.UpdateSubscriptionPermissions,
	resolver func(participantID livekit.ParticipantID) types.Participant,
) error {
	return p.uptrackManager.UpdateSubscriptionPermissions(permissions, resolver)
}

func (p *ParticipantImpl) SubscriptionPermissionUpdate(publisherID livekit.ParticipantID, trackID livekit.TrackID, allowed bool) {
	p.lock.Lock()
	if allowed {
		delete(p.disallowedSubscriptions, trackID)
	} else {
		p.disallowedSubscriptions[trackID] = publisherID
	}
	p.lock.Unlock()

	err := p.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_SubscriptionPermissionUpdate{
			SubscriptionPermissionUpdate: &livekit.SubscriptionPermissionUpdate{
				ParticipantSid: string(publisherID),
				TrackSid:       string(trackID),
				Allowed:        allowed,
			},
		},
	})
	if err != nil {
		p.params.Logger.Errorw("could not send subscription permission update", err)
	}
}

func (p *ParticipantImpl) UpdateSubscribedQuality(nodeID string, trackID livekit.TrackID, maxQuality livekit.VideoQuality) error {
	return p.uptrackManager.UpdateSubscribedQuality(nodeID, trackID, maxQuality)
}

func (p *ParticipantImpl) setupUptrackManager() {
	p.uptrackManager = NewUptrackManager(UptrackManagerParams{
		Identity:       p.params.Identity,
		SID:            p.params.SID,
		Config:         p.params.Config,
		AudioConfig:    p.params.AudioConfig,
		Telemetry:      p.params.Telemetry,
		ThrottleConfig: p.params.ThrottleConfig,
		Logger:         p.params.Logger,
	})

	p.uptrackManager.OnTrackPublished(func(track types.PublishedTrack) {
		if p.onTrackPublished != nil {
			p.onTrackPublished(p, track)
		}
	})

	p.uptrackManager.OnTrackUpdated(func(track types.PublishedTrack, onlyIfReady bool) {
		if onlyIfReady && !p.IsReady() {
			return
		}

		if p.onTrackUpdated != nil {
			p.onTrackUpdated(p, track)
		}
	})

	p.uptrackManager.OnWriteRTCP(func(pkts []rtcp.Packet) {
		if err := p.publisher.pc.WriteRTCP(pkts); err != nil {
			p.params.Logger.Errorw("could not write RTCP to participant", err)
		}
	})

	p.uptrackManager.OnSubscribedMaxQualityChange(p.onSubscribedMaxQualityChange)
}

func (p *ParticipantImpl) sendIceCandidate(c *webrtc.ICECandidate, target livekit.SignalTarget) {
	ci := c.ToJSON()

	// write candidate
	p.params.Logger.Debugw("sending ice candidates",
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
	p.params.Logger.Debugw("updating participant state", "state", state.String())
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
	if sink == nil {
		return nil
	}
	err := sink.WriteMessage(msg)
	if err != nil {
		p.params.Logger.Warnw("could not send message to participant", err,
			"message", fmt.Sprintf("%T", msg.Message))
		return err
	}
	return nil
}

// when the server has an offer for participant
func (p *ParticipantImpl) onOffer(offer webrtc.SessionDescription) {
	if p.State() == livekit.ParticipantInfo_DISCONNECTED {
		// skip when disconnected
		return
	}

	p.params.Logger.Debugw("sending server offer to participant")

	err := p.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_Offer{
			Offer: ToProtoSessionDescription(offer),
		},
	})
	if err != nil {
		prometheus.ServiceOperationCounter.WithLabelValues("offer", "error", "write_message").Add(1)
	} else {
		prometheus.ServiceOperationCounter.WithLabelValues("offer", "success", "").Add(1)
	}
}

// when a new remoteTrack is created, creates a Track and adds it to room
func (p *ParticipantImpl) onMediaTrack(track *webrtc.TrackRemote, rtpReceiver *webrtc.RTPReceiver) {
	if p.State() == livekit.ParticipantInfo_DISCONNECTED {
		return
	}

	p.params.Logger.Debugw("mediaTrack added",
		"kind", track.Kind().String(),
		"track", track.ID(),
		"rid", track.RID(),
		"SSRC", track.SSRC())

	if !p.CanPublish() {
		p.params.Logger.Warnw("no permission to publish mediaTrack", nil)
		return
	}

	p.uptrackManager.MediaTrackReceived(track, rtpReceiver)
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
		p.params.Logger.Warnw("unsupported datachannel added", nil, "label", dc.Label())
	}
}

func (p *ParticipantImpl) handleDataMessage(kind livekit.DataPacket_Kind, data []byte) {
	dp := livekit.DataPacket{}
	if err := proto.Unmarshal(data, &dp); err != nil {
		p.params.Logger.Warnw("could not parse data packet", err)
		return
	}

	// trust the channel that it came in as the source of truth
	dp.Kind = kind

	// only forward on user payloads
	switch payload := dp.Value.(type) {
	case *livekit.DataPacket_User:
		if p.onDataPacket != nil {
			payload.User.ParticipantSid = string(p.params.SID)
			p.onDataPacket(p, &dp)
		}
	default:
		p.params.Logger.Warnw("received unsupported data packet", nil, "payload", payload)
	}
}

func (p *ParticipantImpl) handlePrimaryICEStateChange(state webrtc.ICEConnectionState) {
	if state == webrtc.ICEConnectionStateConnected {
		prometheus.ServiceOperationCounter.WithLabelValues("ice_connection", "success", "").Add(1)
		p.updateState(livekit.ParticipantInfo_ACTIVE)
	} else if state == webrtc.ICEConnectionStateFailed {
		// only close when failed, to allow clients opportunity to reconnect
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
		for _, subTrack := range p.subscribedTracks {
			sr := subTrack.DownTrack().CreateSenderReport()
			chunks := subTrack.DownTrack().CreateSourceDescriptionChunks()
			if sr == nil || chunks == nil {
				continue
			}
			srs = append(srs, sr)
			sd = append(sd, chunks...)
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
					logger.Errorw("could not send downtrack reports", err)
				}
			}

			pkts = pkts[:0]
			batchSize = 0
		}
	}
}

func (p *ParticipantImpl) configureReceiverDTX() {
	//
	// DTX (Discontinuous Transmission) allows audio bandwidth saving
	// by not sending packets during silence periods.
	//
	// Publisher side DTX can enabled by included `usedtx=1` in
	// the `fmtp` line corresponding to audio codec (Opus) in SDP.
	// By doing this in the SDP `answer`, it can be controlled from
	// server side and avoid doing it in all the client SDKs.
	//
	// Ideally, a publisher should be able to specify per audio
	// track if DTX should be enabled. But, translating the
	// DTX preference of publisher to the correct transceiver
	// is non-deterministic due to the lack of a synchronizing id
	// like the track id. The codec preference to set DTX needs
	// to be done
	//   - after calling `SetRemoteDescription` which sets up
	//     the transceivers, but there are no tracks in the
	//     transceiver yet
	//   - before calling `CreateAnswer`
	// Due to the absence of tracks when it is required to set DTX,
	// it is not possible to cross reference against a pending track
	// with the same track id.
	//
	// Due to the restriction above and given that in practice
	// most of the time there is going to be only one audio track
	// that is published, do the following
	//    - if there is no pending audio track, no-op
	//    - if there are no audio transceivers without tracks, no-op
	//    - else, apply the DTX setting from pending audio track
	//      to the audio transceiver without no tracks
	//
	// NOTE: The above logic will fail if there is an `offer` SDP with
	// multiple audio tracks. At that point, there might be a need to
	// rely on something like order of tracks. TODO
	//
	enableDTX := p.uptrackManager.GetDTX()
	transceivers := p.publisher.pc.GetTransceivers()
	for _, transceiver := range transceivers {
		if transceiver.Kind() != webrtc.RTPCodecTypeAudio {
			continue
		}

		receiver := transceiver.Receiver()
		if receiver == nil || receiver.Track() != nil {
			continue
		}

		var modifiedReceiverCodecs []webrtc.RTPCodecParameters

		receiverCodecs := receiver.GetParameters().Codecs
		for _, receiverCodec := range receiverCodecs {
			if receiverCodec.MimeType == webrtc.MimeTypeOpus {
				fmtpUseDTX := "usedtx=1"
				// remove occurrence in the middle
				sdpFmtpLine := strings.ReplaceAll(receiverCodec.SDPFmtpLine, fmtpUseDTX+";", "")
				// remove occurrence at the end
				sdpFmtpLine = strings.ReplaceAll(sdpFmtpLine, fmtpUseDTX, "")
				if enableDTX {
					sdpFmtpLine += ";" + fmtpUseDTX
				}
				receiverCodec.SDPFmtpLine = sdpFmtpLine
			}
			modifiedReceiverCodecs = append(modifiedReceiverCodecs, receiverCodec)
		}

		//
		// As `SetCodecPreferences` on a transceiver replaces all codecs,
		// cycle through sender codecs also and add them before calling
		// `SetCodecPreferences`
		//
		var senderCodecs []webrtc.RTPCodecParameters
		sender := transceiver.Sender()
		if sender != nil {
			senderCodecs = sender.GetParameters().Codecs
		}

		err := transceiver.SetCodecPreferences(append(modifiedReceiverCodecs, senderCodecs...))
		if err != nil {
			p.params.Logger.Warnw("failed to SetCodecPreferences", err)
		}
	}
}

func (p *ParticipantImpl) onStreamStateChange(update *sfu.StreamStateUpdate) error {
	if len(update.StreamStates) == 0 {
		return nil
	}

	streamStateUpdate := &livekit.StreamStateUpdate{}
	for _, streamStateInfo := range update.StreamStates {
		state := livekit.StreamState_ACTIVE
		if streamStateInfo.State == sfu.StreamStatePaused {
			state = livekit.StreamState_PAUSED
		}
		streamStateUpdate.StreamStates = append(streamStateUpdate.StreamStates, &livekit.StreamStateInfo{
			ParticipantSid: string(streamStateInfo.ParticipantID),
			TrackSid:       string(streamStateInfo.TrackID),
			State:          state,
		})
	}

	return p.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_StreamStateUpdate{
			StreamStateUpdate: streamStateUpdate,
		},
	})
}

func (p *ParticipantImpl) onSubscribedMaxQualityChange(trackID livekit.TrackID, subscribedQualities []*livekit.SubscribedQuality) error {
	if len(subscribedQualities) == 0 {
		return nil
	}

	subscribedQualityUpdate := &livekit.SubscribedQualityUpdate{
		TrackSid:            string(trackID),
		SubscribedQualities: subscribedQualities,
	}

	return p.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_SubscribedQualityUpdate{
			SubscribedQualityUpdate: subscribedQualityUpdate,
		},
	})
}

func (p *ParticipantImpl) DebugInfo() map[string]interface{} {
	info := map[string]interface{}{
		"ID":    p.params.SID,
		"State": p.State().String(),
	}

	uptrackManagerInfo := p.uptrackManager.DebugInfo()

	subscribedTrackInfo := make(map[livekit.TrackID]interface{})
	p.lock.RLock()
	for _, track := range p.subscribedTracks {
		dt := track.DownTrack().DebugInfo()
		dt["SubMuted"] = track.IsMuted()
		subscribedTrackInfo[track.ID()] = dt
	}
	p.lock.RUnlock()

	info["UptrackManager"] = uptrackManagerInfo
	info["SubscribedTracks"] = subscribedTrackInfo

	return info
}
