package rtc

import (
	"context"
	"os"
	"time"

	"github.com/livekit/mediatransportutil/pkg/twcc"
	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"
	"go.uber.org/atomic"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/rtc/relay"
	"github.com/livekit/livekit-server/pkg/rtc/supervisor"
	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/sfu"
	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/sfu/connectionquality"
	"github.com/livekit/livekit-server/pkg/telemetry"
)

type RelayedParticipantParams struct {
	Identity          livekit.ParticipantIdentity
	Name              livekit.ParticipantName
	SID               livekit.ParticipantID
	Config            *WebRTCConfig
	AudioConfig       config.AudioConfig
	VideoConfig       config.VideoConfig
	Logger            logger.Logger
	SimTracks         map[uint32]SimulcastTrackInfo
	InitialVersion    uint32
	Telemetry         telemetry.TelemetryService
	PLIThrottleConfig config.PLIThrottleConfig
	VersionGenerator  utils.TimedVersionGenerator
	Relay             relay.Relay
}

type RelayedParticipantImpl struct {
	params RelayedParticipantParams

	grants *auth.ClaimGrants

	connectedAt time.Time

	rtcpCh chan []rtcp.Packet

	twcc *twcc.Responder

	*UpTrackManager

	lock    utils.RWMutex
	version atomic.Uint32

	// callbacks & handlers
	onTrackPublished     func(types.LocalParticipant, types.MediaTrack)
	onTrackUpdated       func(types.LocalParticipant, types.MediaTrack)
	onTrackUnpublished   func(types.LocalParticipant, types.MediaTrack)
	onMigrateStateChange func(p types.LocalParticipant, migrateState types.MigrateState)

	migrateState atomic.Value // types.MigrateState

	supervisor *supervisor.ParticipantSupervisor
}

func NewRelayedParticipant(params RelayedParticipantParams) (*RelayedParticipantImpl, error) {
	if params.Identity == "" {
		return nil, ErrEmptyIdentity
	}
	if params.SID == "" {
		return nil, ErrEmptyParticipantID
	}

	t := true
	p := &RelayedParticipantImpl{
		params: params,
		rtcpCh: make(chan []rtcp.Packet, 100),
		grants: &auth.ClaimGrants{
			Identity: string(params.Identity),
			Name:     string(params.Name),
			Video: &auth.VideoGrant{
				CanPublish:        &t,
				CanPublishData:    &t,
				CanPublishSources: []string{"camera", "microphone", "screen_share", "screen_share_audio"},
			},
		},
		connectedAt: time.Now(),
		supervisor:  supervisor.NewParticipantSupervisor(supervisor.ParticipantSupervisorParams{Logger: params.Logger}),
	}
	p.version.Store(params.InitialVersion)

	go p.publisherRTCPWorker()

	p.setupUpTrackManager()

	return p, nil
}

func (p *RelayedParticipantImpl) setupUpTrackManager() {
	p.UpTrackManager = NewUpTrackManager(UpTrackManagerParams{
		SID:              p.params.SID,
		Logger:           p.params.Logger,
		VersionGenerator: p.params.VersionGenerator,
	})

	p.UpTrackManager.OnPublishedTrackUpdated(func(track types.MediaTrack) {
		p.lock.RLock()
		onTrackUpdated := p.onTrackUpdated
		p.lock.RUnlock()
		if onTrackUpdated != nil {
			onTrackUpdated(p, track)
		}
	})

	// TODO
	// p.UpTrackManager.OnUpTrackManagerClose(p.onUpTrackManagerClose)
}

func (p *RelayedParticipantImpl) ID() livekit.ParticipantID {
	return p.params.SID
}

func (p *RelayedParticipantImpl) Identity() livekit.ParticipantIdentity {
	return p.params.Identity
}

func (p *RelayedParticipantImpl) State() livekit.ParticipantInfo_State {
	return livekit.ParticipantInfo_ACTIVE
}

func (p *RelayedParticipantImpl) ToProto() *livekit.ParticipantInfo {
	p.lock.RLock()
	info := &livekit.ParticipantInfo{
		Sid:         string(p.params.SID),
		Identity:    string(p.params.Identity),
		Name:        p.grants.Name,
		State:       p.State(),
		JoinedAt:    p.ConnectedAt().Unix(),
		Version:     p.version.Inc(),
		Permission:  p.grants.Video.ToPermission(),
		Metadata:    p.grants.Metadata,
		IsPublisher: p.IsPublisher(),
	}
	p.lock.RUnlock()
	info.Tracks = p.UpTrackManager.ToProto()

	return info
}

func (p *RelayedParticipantImpl) SetName(name string) {
	// TODO implement me
}

func (p *RelayedParticipantImpl) SetMetadata(metadata string) {
	// TODO implement me
}

func (p *RelayedParticipantImpl) HasPermission(trackID livekit.TrackID, subIdentity livekit.ParticipantIdentity) bool {
	return true
}

func (p *RelayedParticipantImpl) Hidden() bool {
	return false
}

func (p *RelayedParticipantImpl) IsRecorder() bool {
	return false
}

func (p *RelayedParticipantImpl) Start() {
	// TODO implement me
}

func (p *RelayedParticipantImpl) Close(sendLeave bool, reason types.ParticipantCloseReason) error {
	// TODO implement me
	return nil
}

func (p *RelayedParticipantImpl) GetLogger() logger.Logger {
	return p.params.Logger
}

func (p *RelayedParticipantImpl) GetAdaptiveStream() bool {
	return false
}

func (p *RelayedParticipantImpl) ProtocolVersion() types.ProtocolVersion {
	return types.CurrentProtocol
}

func (p *RelayedParticipantImpl) ConnectedAt() time.Time {
	return p.connectedAt
}

func (p *RelayedParticipantImpl) IsClosed() bool {
	// TODO implement me
	return false
}

func (p *RelayedParticipantImpl) IsReady() bool {
	// TODO implement me
	return true
}

func (p *RelayedParticipantImpl) IsDisconnected() bool {
	// TODO implement me
	return false
}

func (p *RelayedParticipantImpl) IsIdle() bool {
	// TODO implement me
	return false
}

func (p *RelayedParticipantImpl) SubscriberAsPrimary() bool {
	// TODO implement me
	return true
}

func (p *RelayedParticipantImpl) GetClientConfiguration() *livekit.ClientConfiguration {
	return nil
}

func (p *RelayedParticipantImpl) GetICEConnectionType() types.ICEConnectionType {
	return types.ICEConnectionTypeUDP
}

func (p *RelayedParticipantImpl) GetBufferFactory() *buffer.Factory {
	// return p.params.Config.BufferFactory
	panic("not implemented")
}

func (p *RelayedParticipantImpl) SetResponseSink(sink routing.MessageSink) {
	// TODO implement me
}

func (p *RelayedParticipantImpl) CloseSignalConnection() {
	// TODO implement me
}

func (p *RelayedParticipantImpl) UpdateLastSeenSignal() {
	// TODO implement me
}

func (p *RelayedParticipantImpl) SetSignalSourceValid(valid bool) {
	// TODO implement me
}

func (p *RelayedParticipantImpl) ClaimGrants() *auth.ClaimGrants {
	p.lock.RLock()
	defer p.lock.RUnlock()

	return p.grants.Clone()
}

func (p *RelayedParticipantImpl) SetPermission(permission *livekit.ParticipantPermission) bool {
	return false
}

func (p *RelayedParticipantImpl) CanPublish() bool {
	p.lock.RLock()
	defer p.lock.RUnlock()

	return p.grants.Video.GetCanPublish()
}

func (p *RelayedParticipantImpl) CanSubscribe() bool {
	p.lock.RLock()
	defer p.lock.RUnlock()

	return p.grants.Video.GetCanSubscribe()
}

func (p *RelayedParticipantImpl) CanPublishData() bool {
	p.lock.RLock()
	defer p.lock.RUnlock()

	return p.grants.Video.GetCanPublishData()
}

func (p *RelayedParticipantImpl) AddICECandidate(candidate webrtc.ICECandidateInit, target livekit.SignalTarget) {
	// TODO implement me
	panic("implement me")
}

func (p *RelayedParticipantImpl) HandleOffer(sdp webrtc.SessionDescription) {
	// TODO implement me
	panic("implement me")
}

func (p *RelayedParticipantImpl) AddTrack(req *livekit.AddTrackRequest) {
	// TODO implement me
	panic("implement me")
}

func (p *RelayedParticipantImpl) SetTrackMuted(trackID livekit.TrackID, muted bool, fromAdmin bool) {
	// TODO implement me
	panic("implement me")
}

func (p *RelayedParticipantImpl) HandleAnswer(sdp webrtc.SessionDescription) {
	// TODO implement me
	panic("implement me")
}

func (p *RelayedParticipantImpl) Negotiate(force bool) {
	// TODO implement me
}

func (p *RelayedParticipantImpl) ICERestart(iceConfig *livekit.ICEConfig, reason livekit.ReconnectReason) {
	// TODO implement me
	panic("implement me")
}

func (p *RelayedParticipantImpl) AddTrackToSubscriber(trackLocal webrtc.TrackLocal, params types.AddTrackParams) (*webrtc.RTPSender, *webrtc.RTPTransceiver, error) {
	// TODO implement me
	panic("implement me")
}

func (p *RelayedParticipantImpl) AddTransceiverFromTrackToSubscriber(trackLocal webrtc.TrackLocal, params types.AddTrackParams) (*webrtc.RTPSender, *webrtc.RTPTransceiver, error) {
	// TODO implement me
	panic("implement me")
}

func (p *RelayedParticipantImpl) RemoveTrackFromSubscriber(sender *webrtc.RTPSender) error {
	// TODO implement me
	panic("implement me")
}

func (p *RelayedParticipantImpl) SubscribeToTrack(trackID livekit.TrackID) {
	// TODO implement me
	panic("implement me")
}

func (p *RelayedParticipantImpl) UnsubscribeFromTrack(trackID livekit.TrackID) {
	// TODO implement me
	panic("implement me")
}

func (p *RelayedParticipantImpl) UpdateSubscribedTrackSettings(trackID livekit.TrackID, settings *livekit.UpdateTrackSettings) {
	// TODO implement me
	panic("implement me")
}

func (p *RelayedParticipantImpl) GetSubscribedTracks() []types.SubscribedTrack {
	return []types.SubscribedTrack{}
}

func (p *RelayedParticipantImpl) VerifySubscribeParticipantInfo(pID livekit.ParticipantID, version uint32) {
	// TODO implement me
	panic("implement me")
}

func (p *RelayedParticipantImpl) WaitUntilSubscribed(timeout time.Duration) error {
	return nil
}

func (p *RelayedParticipantImpl) GetSubscribedParticipants() []livekit.ParticipantID {
	return []livekit.ParticipantID{}
}

func (p *RelayedParticipantImpl) IsSubscribedTo(sid livekit.ParticipantID) bool {
	return false
}

func (p *RelayedParticipantImpl) IsPublisher() bool {
	return true
}

func (p *RelayedParticipantImpl) GetAudioLevel() (level float64, active bool) {
	level = 0
	for _, pt := range p.GetPublishedTracks() {
		mediaTrack := pt.(types.LocalMediaTrack)
		if mediaTrack.Source() == livekit.TrackSource_MICROPHONE {
			tl, ta := mediaTrack.GetAudioLevel()
			if ta {
				active = true
				if tl > level {
					level = tl
				}
			}
		}
	}
	return
}

func (p *RelayedParticipantImpl) GetConnectionQuality() *livekit.ConnectionQualityInfo {
	numTracks := 0
	minQuality := livekit.ConnectionQuality_EXCELLENT
	minScore := float32(0.0)

	for _, pt := range p.GetPublishedTracks() {
		numTracks++

		score, quality := pt.(types.LocalMediaTrack).GetConnectionScoreAndQuality()
		if quality < minQuality {
			// WARNING NOTE: comparing protobuf enums directly
			minQuality = quality
			minScore = score
		} else if quality == minQuality && score < minScore {
			minScore = score
		}
	}

	if numTracks == 0 {
		minQuality = livekit.ConnectionQuality_EXCELLENT
		minScore = connectionquality.MaxMOS
	}

	return &livekit.ConnectionQualityInfo{
		ParticipantSid: string(p.ID()),
		Quality:        minQuality,
		Score:          minScore,
	}
}

func (p *RelayedParticipantImpl) SendJoinResponse(joinResponse *livekit.JoinResponse) error {
	return nil
}

func (p *RelayedParticipantImpl) SendParticipantUpdate(participants []*livekit.ParticipantInfo) error {
	return nil
}

func (p *RelayedParticipantImpl) SendSpeakerUpdate(speakers []*livekit.SpeakerInfo, force bool) error {
	return nil
}

func (p *RelayedParticipantImpl) SendDataPacket(packet *livekit.DataPacket, data []byte) error {
	return nil
}

func (p *RelayedParticipantImpl) SendRoomUpdate(room *livekit.Room) error {
	return nil
}

func (p *RelayedParticipantImpl) SendConnectionQualityUpdate(update *livekit.ConnectionQualityUpdate) error {
	return nil
}

func (p *RelayedParticipantImpl) SubscriptionPermissionUpdate(publisherID livekit.ParticipantID, trackID livekit.TrackID, allowed bool) {
	// TODO implement me
}

func (p *RelayedParticipantImpl) SendRefreshToken(token string) error {
	return nil
}

func (p *RelayedParticipantImpl) SendReconnectResponse(reconnectResponse *livekit.ReconnectResponse) error {
	return nil
}

func (p *RelayedParticipantImpl) IssueFullReconnect(reason types.ParticipantCloseReason) {}

func (p *RelayedParticipantImpl) OnStateChange(f func(p types.LocalParticipant, oldState livekit.ParticipantInfo_State)) {
	// TODO implement me
}

func (p *RelayedParticipantImpl) OnMigrateStateChange(f func(p types.LocalParticipant, migrateState types.MigrateState)) {
	// TODO implement me
}

func (p *RelayedParticipantImpl) OnTrackPublished(callback func(types.LocalParticipant, types.MediaTrack)) {
	p.lock.Lock()
	p.onTrackPublished = callback
	p.lock.Unlock()
}

func (p *RelayedParticipantImpl) OnTrackUpdated(callback func(types.LocalParticipant, types.MediaTrack)) {
	p.lock.Lock()
	p.onTrackUpdated = callback
	p.lock.Unlock()
}

func (p *RelayedParticipantImpl) OnTrackUnpublished(callback func(types.LocalParticipant, types.MediaTrack)) {
	p.lock.Lock()
	p.onTrackUnpublished = callback
	p.lock.Unlock()
}

func (p *RelayedParticipantImpl) OnParticipantUpdate(callback func(types.LocalParticipant)) {
	// TODO implement me
}

func (p *RelayedParticipantImpl) OnDataPacket(callback func(types.LocalParticipant, *livekit.DataPacket)) {
	// TODO implement me
}

func (p *RelayedParticipantImpl) OnSubscribeStatusChanged(fn func(publisherID livekit.ParticipantID, subscribed bool)) {
	// TODO implement me
}

func (p *RelayedParticipantImpl) OnClose(callback func(types.LocalParticipant, map[livekit.TrackID]livekit.ParticipantID)) {
	// TODO implement me
}

func (p *RelayedParticipantImpl) OnClaimsChanged(callback func(types.LocalParticipant)) {
	// TODO implement me
}

func (p *RelayedParticipantImpl) OnReceiverReport(dt *sfu.DownTrack, report *rtcp.ReceiverReport) {}

func (p *RelayedParticipantImpl) MaybeStartMigration(force bool, onStart func()) bool {
	// TODO implement me
	return false
}

func (p *RelayedParticipantImpl) SetMigrateState(s types.MigrateState) {
	preState := p.MigrateState()
	if preState == types.MigrateStateComplete || preState == s {
		return
	}

	p.params.Logger.Debugw("SetMigrateState", "state", s)
	p.migrateState.Store(s)

	if onMigrateStateChange := p.getOnMigrateStateChange(); onMigrateStateChange != nil {
		go onMigrateStateChange(p, s)
	}
}

func (p *RelayedParticipantImpl) MigrateState() types.MigrateState {
	// TODO implement me
	return types.MigrateStateInit
}

func (p *RelayedParticipantImpl) SetMigrateInfo(previousOffer, previousAnswer *webrtc.SessionDescription, mediaTracks []*livekit.TrackPublishedResponse, dataChannels []*livekit.DataChannelInfo) {
	// TODO implement me
}

func (p *RelayedParticipantImpl) UpdateMediaRTT(rtt uint32) {
	// TODO implement me
	panic("implement me")
}

func (p *RelayedParticipantImpl) UpdateSignalingRTT(rtt uint32) {
	// TODO implement me
	panic("implement me")
}

func (p *RelayedParticipantImpl) CacheDownTrack(trackID livekit.TrackID, rtpTransceiver *webrtc.RTPTransceiver, downTrackState sfu.DownTrackState) {
	// TODO implement me
	panic("implement me")
}

func (p *RelayedParticipantImpl) UncacheDownTrack(rtpTransceiver *webrtc.RTPTransceiver) {
	// TODO implement me
	panic("implement me")
}

func (p *RelayedParticipantImpl) GetCachedDownTrack(trackID livekit.TrackID) (*webrtc.RTPTransceiver, sfu.DownTrackState) {
	// TODO implement me
	panic("implement me")
}

func (p *RelayedParticipantImpl) SetICEConfig(iceConfig *livekit.ICEConfig) {
	// TODO implement me
	panic("implement me")
}

func (p *RelayedParticipantImpl) OnICEConfigChanged(callback func(participant types.LocalParticipant, iceConfig *livekit.ICEConfig)) {
	// TODO implement me
	panic("implement me")
}

func (p *RelayedParticipantImpl) UpdateSubscribedQuality(nodeID livekit.NodeID, trackID livekit.TrackID, maxQualities []types.SubscribedCodecQuality) error {
	// TODO implement me
	panic("implement me")
}

func (p *RelayedParticipantImpl) UpdateMediaLoss(nodeID livekit.NodeID, trackID livekit.TrackID, fractionalLoss uint32) error {
	// TODO implement me
	panic("implement me")
}

func (p *RelayedParticipantImpl) OnMediaTrack(track *webrtc.TrackRemote, rtpReceiver *webrtc.RTPReceiver, mid, rid string, tracks []*livekit.TrackInfo) {
	if p.IsDisconnected() {
		return
	}

	if !p.CanPublish() {
		p.params.Logger.Warnw("no permission to publish mediaTrack", nil)
		return
	}

	publishedTrack, isNewTrack := p.mediaTrackReceived(track, rtpReceiver, mid, rid, tracks)

	if publishedTrack != nil {
		p.params.Logger.Infow("mediaTrack published",
			"kind", track.Kind().String(),
			"trackID", publishedTrack.ID(),
			"rid", rid,
			"SSRC", track.SSRC(),
			"mime", track.Codec().MimeType,
		)
	} else {
		p.params.Logger.Warnw("webrtc Track published but can't find MediaTrack", nil,
			"kind", track.Kind().String(),
			"webrtcTrackID", track.ID(),
			"rid", rid,
			"SSRC", track.SSRC(),
			"mime", track.Codec().MimeType,
		)
	}

	if !isNewTrack && publishedTrack != nil && !publishedTrack.HasPendingCodec() && p.IsReady() {
		p.lock.RLock()
		onTrackUpdated := p.onTrackUpdated
		p.lock.RUnlock()
		if onTrackUpdated != nil {
			onTrackUpdated(p, publishedTrack)
		}
	}
}

func (p *RelayedParticipantImpl) mediaTrackReceived(track *webrtc.TrackRemote, rtpReceiver *webrtc.RTPReceiver, mid string, rid string, trackInfos []*livekit.TrackInfo) (*MediaTrack, bool) {
	newTrack := false

	p.params.Logger.Debugw(
		"media track received",
		"kind", track.Kind().String(),
		"trackID", track.ID(),
		"rid", rid,
		"SSRC", track.SSRC(),
		"mime", track.Codec().MimeType,
	)

	if mid == "" {
		p.params.Logger.Warnw("could not get mid for track", nil, "trackID", track.ID())
		return nil, false
	}

	// use existing media track to handle simulcast
	mt, ok := p.getPublishedTrackBySdpCid(track.ID()).(*MediaTrack)
	if !ok {
		var ti *livekit.TrackInfo

		// TODO
		for _, trackInfo := range trackInfos {
			if trackInfo.Type == livekit.TrackType_VIDEO && track.Kind() == webrtc.RTPCodecTypeVideo ||
				trackInfo.Type == livekit.TrackType_AUDIO && track.Kind() == webrtc.RTPCodecTypeAudio {
				ti = trackInfo
			}
		}

		if ti == nil {
			panic("ti == nil")
		}

		ti.MimeType = track.Codec().MimeType
		// TODO: investigate difference between signalCid and sdpCid
		mt = p.addMediaTrack(track.ID(), track.ID(), ti)
		newTrack = true
	}

	ssrc := uint32(track.SSRC())
	if p.twcc == nil {
		p.twcc = twcc.NewTransportWideCCResponder(ssrc)
		p.twcc.OnFeedback(func(pkt rtcp.RawPacket) {
			p.postRtcp([]rtcp.Packet{&pkt})
		})
	}

	if mt.AddReceiver(rtpReceiver, track, rid, p.twcc, mid) {
		// TODO p.removeMutedTrackNotFired(mt)
		if newTrack {
			go p.handleTrackPublished(mt)
		}
	}

	return mt, newTrack
}

func (p *RelayedParticipantImpl) getPublishedTrackBySdpCid(clientId string) types.MediaTrack {
	for _, publishedTrack := range p.GetPublishedTracks() {
		if publishedTrack.(types.LocalMediaTrack).HasSdpCid(clientId) {
			p.params.Logger.Debugw("found track by sdp cid", "sdpCid", clientId, "trackID", publishedTrack.ID())
			return publishedTrack
		}
	}

	return nil
}

func (p *RelayedParticipantImpl) addMediaTrack(signalCid string, sdpCid string, ti *livekit.TrackInfo) *MediaTrack {
	mt := NewMediaTrack(MediaTrackParams{
		TrackInfo:           proto.Clone(ti).(*livekit.TrackInfo),
		SignalCid:           signalCid,
		SdpCid:              sdpCid,
		ParticipantID:       p.params.SID,
		ParticipantIdentity: p.params.Identity,
		ParticipantVersion:  p.version.Load(),
		RTCPChan:            p.rtcpCh,
		BufferFactory:       p.params.Config.BufferFactory,
		ReceiverConfig:      p.params.Config.Receiver,
		AudioConfig:         p.params.AudioConfig,
		VideoConfig:         p.params.VideoConfig,
		Telemetry:           p.params.Telemetry,
		Logger:              LoggerWithTrack(p.params.Logger, livekit.TrackID(ti.Sid), false),
		SubscriberConfig:    p.params.Config.Subscriber,
		PLIThrottleConfig:   p.params.PLIThrottleConfig,
		SimTracks:           p.params.SimTracks,
	})

	// TODO mt.OnSubscribedMaxQualityChange(p.onSubscribedMaxQualityChange)

	// add to published and clean up pending
	p.supervisor.SetPublishedTrack(livekit.TrackID(ti.Sid), mt)
	p.UpTrackManager.AddPublishedTrack(mt)

	// pti := p.pendingTracks[signalCid]
	// if pti != nil {
	//	if p.pendingPublishingTracks[livekit.TrackID(ti.Sid)] != nil {
	//		p.params.Logger.Infow("unexpected pending publish track", "trackID", ti.Sid)
	//	}
	//	p.pendingPublishingTracks[livekit.TrackID(ti.Sid)] = &pendingTrackInfo{
	//		trackInfos: []*livekit.TrackInfo{pti.trackInfos[0]},
	//		migrated:   pti.migrated,
	//	}
	// }
	//
	// p.pendingTracks[signalCid].trackInfos = p.pendingTracks[signalCid].trackInfos[1:]
	// if len(p.pendingTracks[signalCid].trackInfos) == 0 {
	//	delete(p.pendingTracks, signalCid)
	// }

	trackID := livekit.TrackID(ti.Sid)
	mt.AddOnClose(func() {
		p.supervisor.ClearPublishedTrack(trackID, mt)

		// not logged when closing
		p.params.Telemetry.TrackUnpublished(
			context.Background(),
			p.ID(),
			p.Identity(),
			mt.ToProto(),
			!p.IsClosed(),
		)

		if !p.IsClosed() {
			// unpublished events aren't necessary when participant is closed
			p.params.Logger.Infow("unpublished track", "trackID", ti.Sid, "trackInfo", ti)
			p.lock.RLock()
			onTrackUnpublished := p.onTrackUnpublished
			p.lock.RUnlock()
			if onTrackUnpublished != nil {
				onTrackUnpublished(p, mt)
			}
		}
	})

	return mt
}

func (p *RelayedParticipantImpl) handleTrackPublished(track types.MediaTrack) {
	p.lock.RLock()
	onTrackPublished := p.onTrackPublished
	p.lock.RUnlock()
	if onTrackPublished != nil {
		onTrackPublished(p, track)
	}

	// send webhook after callbacks are complete, persistence and state handling happens
	// in `onTrackPublished` cb
	p.params.Telemetry.TrackPublished(
		context.Background(),
		p.ID(),
		p.Identity(),
		track.ToProto(),
	)

	if !p.hasPendingMigratedTrack() {
		p.SetMigrateState(types.MigrateStateComplete)
	}
}

func (p *RelayedParticipantImpl) hasPendingMigratedTrack() bool {
	return false
}

func (p *RelayedParticipantImpl) getOnMigrateStateChange() func(p types.LocalParticipant, state types.MigrateState) {
	p.lock.RLock()
	defer p.lock.RUnlock()

	return p.onMigrateStateChange
}

func (p *RelayedParticipantImpl) publisherRTCPWorker() {
	defer func() {
		if r := Recover(p.GetLogger()); r != nil {
			os.Exit(1)
		}
	}()

	// read from rtcpChan
	for pkts := range p.rtcpCh {
		if pkts == nil {
			p.params.Logger.Debugw("exiting publisher RTCP worker")
			return
		}

		if err := p.params.Relay.WriteRTCP(pkts); err != nil {
			p.params.Logger.Errorw("could not write RTCP to participant", err)
		}
	}
}

func (p *RelayedParticipantImpl) postRtcp(pkts []rtcp.Packet) {
	select {
	case p.rtcpCh <- pkts:
	default:
		p.params.Logger.Warnw("rtcp channel full", nil)
	}
}
