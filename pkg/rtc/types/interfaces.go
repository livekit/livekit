package types

import (
	"fmt"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils"

	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/sfu"
	"github.com/livekit/livekit-server/pkg/sfu/buffer"
)

//go:generate go run github.com/maxbrunsfeld/counterfeiter/v6 -generate

//counterfeiter:generate . WebsocketClient
type WebsocketClient interface {
	ReadMessage() (messageType int, p []byte, err error)
	WriteMessage(messageType int, data []byte) error
	WriteControl(messageType int, data []byte, deadline time.Time) error
}

type AddSubscriberParams struct {
	AllTracks bool
	TrackIDs  []livekit.TrackID
}

// ---------------------------------------------

type MigrateState int32

const (
	MigrateStateInit MigrateState = iota
	MigrateStateSync
	MigrateStateComplete
)

func (m MigrateState) String() string {
	switch m {
	case MigrateStateInit:
		return "MIGRATE_STATE_INIT"
	case MigrateStateSync:
		return "MIGRATE_STATE_SYNC"
	case MigrateStateComplete:
		return "MIGRATE_STATE_COMPLETE"
	default:
		return fmt.Sprintf("%d", int(m))
	}
}

// ---------------------------------------------

type SubscribedCodecQuality struct {
	CodecMime string
	Quality   livekit.VideoQuality
}

// ---------------------------------------------

type ParticipantCloseReason int

const (
	ParticipantCloseReasonClientRequestLeave ParticipantCloseReason = iota
	ParticipantCloseReasonRoomManagerStop
	ParticipantCloseReasonRoomClose
	ParticipantCloseReasonVerifyFailed
	ParticipantCloseReasonJoinFailed
	ParticipantCloseReasonJoinTimeout
	ParticipantCloseReasonStateDisconnected
	ParticipantCloseReasonPeerConnectionDisconnected
	ParticipantCloseReasonDuplicateIdentity
	ParticipantCloseReasonMigrationComplete
	ParticipantCloseReasonStale
	ParticipantCloseReasonServiceRequestRemoveParticipant
	ParticipantCloseReasonServiceRequestDeleteRoom
	ParticipantCloseReasonSimulateMigration
	ParticipantCloseReasonSimulateNodeFailure
	ParticipantCloseReasonSimulateServerLeave
	ParticipantCloseReasonNegotiateFailed
	ParticipantCloseReasonMigrationRequested
	ParticipantCloseReasonOvercommitted
	ParticipantCloseReasonPublicationError
)

func (p ParticipantCloseReason) String() string {
	switch p {
	case ParticipantCloseReasonClientRequestLeave:
		return "CLIENT_REQUEST_LEAVE"
	case ParticipantCloseReasonRoomManagerStop:
		return "ROOM_MANAGER_STOP"
	case ParticipantCloseReasonRoomClose:
		return "ROOM_CLOSE"
	case ParticipantCloseReasonVerifyFailed:
		return "VERIFY_FAILED"
	case ParticipantCloseReasonJoinFailed:
		return "JOIN_FAILED"
	case ParticipantCloseReasonJoinTimeout:
		return "JOIN_TIMEOUT"
	case ParticipantCloseReasonStateDisconnected:
		return "STATE_DISCONNECTED"
	case ParticipantCloseReasonPeerConnectionDisconnected:
		return "PEER_CONNECTION_DISCONNECTED"
	case ParticipantCloseReasonDuplicateIdentity:
		return "DUPLICATE_IDENTITY"
	case ParticipantCloseReasonMigrationComplete:
		return "MIGRATION_COMPLETE"
	case ParticipantCloseReasonStale:
		return "STALE"
	case ParticipantCloseReasonServiceRequestRemoveParticipant:
		return "SERVICE_REQUEST_REMOVE_PARTICIPANT"
	case ParticipantCloseReasonServiceRequestDeleteRoom:
		return "SERVICE_REQUEST_DELETE_ROOM"
	case ParticipantCloseReasonSimulateMigration:
		return "SIMULATE_MIGRATION"
	case ParticipantCloseReasonSimulateNodeFailure:
		return "SIMULATE_NODE_FAILURE"
	case ParticipantCloseReasonSimulateServerLeave:
		return "SIMULATE_SERVER_LEAVE"
	case ParticipantCloseReasonNegotiateFailed:
		return "NEGOTIATE_FAILED"
	case ParticipantCloseReasonMigrationRequested:
		return "MIGRATION_REQUESTED"
	case ParticipantCloseReasonOvercommitted:
		return "OVERCOMMITTED"
	case ParticipantCloseReasonPublicationError:
		return "PUBLICATION_ERROR"
	default:
		return fmt.Sprintf("%d", int(p))
	}
}

func (p ParticipantCloseReason) ToDisconnectReason() livekit.DisconnectReason {
	switch p {
	case ParticipantCloseReasonClientRequestLeave:
		return livekit.DisconnectReason_CLIENT_INITIATED
	case ParticipantCloseReasonRoomManagerStop:
		return livekit.DisconnectReason_SERVER_SHUTDOWN
	case ParticipantCloseReasonVerifyFailed, ParticipantCloseReasonJoinFailed, ParticipantCloseReasonJoinTimeout:
		// expected to be connected but is not
		return livekit.DisconnectReason_JOIN_FAILURE
	case ParticipantCloseReasonPeerConnectionDisconnected:
		return livekit.DisconnectReason_STATE_MISMATCH
	case ParticipantCloseReasonDuplicateIdentity, ParticipantCloseReasonMigrationComplete, ParticipantCloseReasonStale:
		return livekit.DisconnectReason_DUPLICATE_IDENTITY
	case ParticipantCloseReasonServiceRequestRemoveParticipant:
		return livekit.DisconnectReason_PARTICIPANT_REMOVED
	case ParticipantCloseReasonServiceRequestDeleteRoom:
		return livekit.DisconnectReason_ROOM_DELETED
	case ParticipantCloseReasonSimulateMigration:
		return livekit.DisconnectReason_DUPLICATE_IDENTITY
	case ParticipantCloseReasonSimulateNodeFailure:
		return livekit.DisconnectReason_SERVER_SHUTDOWN
	case ParticipantCloseReasonSimulateServerLeave:
		return livekit.DisconnectReason_SERVER_SHUTDOWN
	case ParticipantCloseReasonOvercommitted:
		return livekit.DisconnectReason_SERVER_SHUTDOWN
	case ParticipantCloseReasonNegotiateFailed, ParticipantCloseReasonPublicationError:
		return livekit.DisconnectReason_STATE_MISMATCH
	default:
		// the other types will map to unknown reason
		return livekit.DisconnectReason_UNKNOWN_REASON
	}
}

// ---------------------------------------------

//counterfeiter:generate . Participant
type Participant interface {
	ID() livekit.ParticipantID
	Identity() livekit.ParticipantIdentity
	State() livekit.ParticipantInfo_State

	ToProto() *livekit.ParticipantInfo

	SetName(name string)
	SetMetadata(metadata string)

	GetPublishedTrack(sid livekit.TrackID) MediaTrack
	GetPublishedTracks() []MediaTrack
	RemovePublishedTrack(track MediaTrack, willBeResumed bool, shouldClose bool)

	// HasPermission checks permission of the subscriber by identity. Returns true if subscriber is allowed to subscribe
	// to the track with trackID
	HasPermission(trackID livekit.TrackID, subIdentity livekit.ParticipantIdentity) bool

	// permissions
	Hidden() bool
	IsRecorder() bool

	Start()
	Close(sendLeave bool, reason ParticipantCloseReason) error

	SubscriptionPermission() (*livekit.SubscriptionPermission, utils.TimedVersion)

	// updates from remotes
	UpdateSubscriptionPermission(
		subscriptionPermission *livekit.SubscriptionPermission,
		timedVersion utils.TimedVersion,
		resolverByIdentity func(participantIdentity livekit.ParticipantIdentity) LocalParticipant,
		resolverBySid func(participantID livekit.ParticipantID) LocalParticipant,
	) error
	UpdateVideoLayers(updateVideoLayers *livekit.UpdateVideoLayers) error

	DebugInfo() map[string]interface{}
}

// -------------------------------------------------------

type ICEConnectionType string

const (
	ICEConnectionTypeUDP     ICEConnectionType = "udp"
	ICEConnectionTypeTCP     ICEConnectionType = "tcp"
	ICEConnectionTypeTURN    ICEConnectionType = "turn"
	ICEConnectionTypeUnknown ICEConnectionType = "unknown"
)

type AddTrackParams struct {
	Stereo bool
	Red    bool
}

//counterfeiter:generate . LocalParticipant
type LocalParticipant interface {
	Participant

	ToProtoWithVersion() (*livekit.ParticipantInfo, utils.TimedVersion)

	// getters
	GetLogger() logger.Logger
	GetAdaptiveStream() bool
	ProtocolVersion() ProtocolVersion
	ConnectedAt() time.Time
	IsClosed() bool
	IsReady() bool
	IsDisconnected() bool
	IsIdle() bool
	SubscriberAsPrimary() bool
	GetClientConfiguration() *livekit.ClientConfiguration
	GetICEConnectionType() ICEConnectionType
	GetBufferFactory() *buffer.Factory

	SetResponseSink(sink routing.MessageSink)
	CloseSignalConnection()
	UpdateLastSeenSignal()
	SetSignalSourceValid(valid bool)

	// permissions
	ClaimGrants() *auth.ClaimGrants
	SetPermission(permission *livekit.ParticipantPermission) bool
	CanPublishSource(source livekit.TrackSource) bool
	CanSubscribe() bool
	CanPublishData() bool

	// PeerConnection
	AddICECandidate(candidate webrtc.ICECandidateInit, target livekit.SignalTarget)
	HandleOffer(sdp webrtc.SessionDescription)
	AddTrack(req *livekit.AddTrackRequest)
	SetTrackMuted(trackID livekit.TrackID, muted bool, fromAdmin bool)

	HandleAnswer(sdp webrtc.SessionDescription)
	Negotiate(force bool)
	ICERestart(iceConfig *livekit.ICEConfig)
	AddTrackToSubscriber(trackLocal webrtc.TrackLocal, params AddTrackParams) (*webrtc.RTPSender, *webrtc.RTPTransceiver, error)
	AddTransceiverFromTrackToSubscriber(trackLocal webrtc.TrackLocal, params AddTrackParams) (*webrtc.RTPSender, *webrtc.RTPTransceiver, error)
	RemoveTrackFromSubscriber(sender *webrtc.RTPSender) error

	// subscriptions
	SubscribeToTrack(trackID livekit.TrackID)
	UnsubscribeFromTrack(trackID livekit.TrackID)
	UpdateSubscribedTrackSettings(trackID livekit.TrackID, settings *livekit.UpdateTrackSettings)
	GetSubscribedTracks() []SubscribedTrack
	VerifySubscribeParticipantInfo(pID livekit.ParticipantID, version uint32)
	// WaitUntilSubscribed waits until all subscriptions have been settled, or if the timeout
	// has been reached. If the timeout expires, it will return an error.
	WaitUntilSubscribed(timeout time.Duration) error

	// returns list of participant identities that the current participant is subscribed to
	GetSubscribedParticipants() []livekit.ParticipantID
	IsSubscribedTo(sid livekit.ParticipantID) bool
	IsPublisher() bool

	GetAudioLevel() (smoothedLevel float64, active bool)
	GetConnectionQuality() *livekit.ConnectionQualityInfo

	// server sent messages
	SendJoinResponse(joinResponse *livekit.JoinResponse) error
	SendParticipantUpdate(participants []*livekit.ParticipantInfo) error
	SendSpeakerUpdate(speakers []*livekit.SpeakerInfo, force bool) error
	SendDataPacket(packet *livekit.DataPacket, data []byte) error
	SendRoomUpdate(room *livekit.Room) error
	SendConnectionQualityUpdate(update *livekit.ConnectionQualityUpdate) error
	SubscriptionPermissionUpdate(publisherID livekit.ParticipantID, trackID livekit.TrackID, allowed bool)
	SendRefreshToken(token string) error
	HandleReconnectAndSendResponse(reconnectReason livekit.ReconnectReason, reconnectResponse *livekit.ReconnectResponse) error
	IssueFullReconnect(reason ParticipantCloseReason)

	// callbacks
	OnStateChange(func(p LocalParticipant, oldState livekit.ParticipantInfo_State))
	OnMigrateStateChange(func(p LocalParticipant, migrateState MigrateState))
	// OnTrackPublished - remote added a track
	OnTrackPublished(func(LocalParticipant, MediaTrack))
	// OnTrackUpdated - one of its publishedTracks changed in status
	OnTrackUpdated(callback func(LocalParticipant, MediaTrack))
	// OnTrackUnpublished - a track was unpublished
	OnTrackUnpublished(callback func(LocalParticipant, MediaTrack))
	// OnParticipantUpdate - metadata or permission is updated
	OnParticipantUpdate(callback func(LocalParticipant))
	OnDataPacket(callback func(LocalParticipant, *livekit.DataPacket))
	OnSubscribeStatusChanged(fn func(publisherID livekit.ParticipantID, subscribed bool))
	OnClose(callback func(LocalParticipant, map[livekit.TrackID]livekit.ParticipantID))
	OnClaimsChanged(callback func(LocalParticipant))
	OnReceiverReport(dt *sfu.DownTrack, report *rtcp.ReceiverReport)

	// session migration
	MaybeStartMigration(force bool, onStart func()) bool
	SetMigrateState(s MigrateState)
	MigrateState() MigrateState
	SetMigrateInfo(previousOffer, previousAnswer *webrtc.SessionDescription, mediaTracks []*livekit.TrackPublishedResponse, dataChannels []*livekit.DataChannelInfo)

	UpdateMediaRTT(rtt uint32)
	UpdateSignalingRTT(rtt uint32)

	CacheDownTrack(trackID livekit.TrackID, rtpTransceiver *webrtc.RTPTransceiver, downTrackState sfu.DownTrackState)
	UncacheDownTrack(rtpTransceiver *webrtc.RTPTransceiver)
	GetCachedDownTrack(trackID livekit.TrackID) (*webrtc.RTPTransceiver, sfu.DownTrackState)

	SetICEConfig(iceConfig *livekit.ICEConfig)
	OnICEConfigChanged(callback func(participant LocalParticipant, iceConfig *livekit.ICEConfig))

	UpdateSubscribedQuality(nodeID livekit.NodeID, trackID livekit.TrackID, maxQualities []SubscribedCodecQuality) error
	UpdateMediaLoss(nodeID livekit.NodeID, trackID livekit.TrackID, fractionalLoss uint32) error

	// down stream bandwidth management
	SetSubscriberAllowPause(allowPause bool)
	SetSubscriberChannelCapacity(channelCapacity int64)
}

// Room is a container of participants, and can provide room-level actions
//
//counterfeiter:generate . Room
type Room interface {
	Name() livekit.RoomName
	ID() livekit.RoomID
	RemoveParticipant(identity livekit.ParticipantIdentity, pID livekit.ParticipantID, reason ParticipantCloseReason)
	UpdateSubscriptions(participant LocalParticipant, trackIDs []livekit.TrackID, participantTracks []*livekit.ParticipantTracks, subscribe bool)
	UpdateSubscriptionPermission(participant LocalParticipant, permissions *livekit.SubscriptionPermission) error
	SyncState(participant LocalParticipant, state *livekit.SyncState) error
	SimulateScenario(participant LocalParticipant, scenario *livekit.SimulateScenario) error
	UpdateVideoLayers(participant Participant, updateVideoLayers *livekit.UpdateVideoLayers) error
	ResolveMediaTrackForSubscriber(subIdentity livekit.ParticipantIdentity, trackID livekit.TrackID) MediaResolverResult
	GetLocalParticipants() []LocalParticipant
}

// MediaTrack represents a media track
//
//counterfeiter:generate . MediaTrack
type MediaTrack interface {
	ID() livekit.TrackID
	Kind() livekit.TrackType
	Name() string
	Source() livekit.TrackSource

	ToProto() *livekit.TrackInfo

	PublisherID() livekit.ParticipantID
	PublisherIdentity() livekit.ParticipantIdentity
	PublisherVersion() uint32

	IsMuted() bool
	SetMuted(muted bool)

	UpdateVideoLayers(layers []*livekit.VideoLayer)
	IsSimulcast() bool

	Close(willBeResumed bool)
	IsOpen() bool

	// callbacks
	AddOnClose(func())

	// subscribers
	AddSubscriber(participant LocalParticipant) (SubscribedTrack, error)
	RemoveSubscriber(participantID livekit.ParticipantID, willBeResumed bool)
	IsSubscriber(subID livekit.ParticipantID) bool
	RevokeDisallowedSubscribers(allowedSubscriberIdentities []livekit.ParticipantIdentity) []livekit.ParticipantIdentity
	GetAllSubscribers() []livekit.ParticipantID
	GetNumSubscribers() int

	// returns quality information that's appropriate for width & height
	GetQualityForDimension(width, height uint32) livekit.VideoQuality

	// returns temporal layer that's appropriate for fps
	GetTemporalLayerForSpatialFps(spatial int32, fps uint32, mime string) int32

	Receivers() []sfu.TrackReceiver
	ClearAllReceivers(willBeResumed bool)
}

//counterfeiter:generate . LocalMediaTrack
type LocalMediaTrack interface {
	MediaTrack

	Restart()

	SignalCid() string
	HasSdpCid(cid string) bool

	GetAudioLevel() (level float64, active bool)
	GetConnectionScoreAndQuality() (float32, livekit.ConnectionQuality)

	SetRTT(rtt uint32)

	NotifySubscriberNodeMaxQuality(nodeID livekit.NodeID, qualities []SubscribedCodecQuality)
	NotifySubscriberNodeMediaLoss(nodeID livekit.NodeID, fractionalLoss uint8)
}

//counterfeiter:generate . SubscribedTrack
type SubscribedTrack interface {
	AddOnBind(f func())
	IsBound() bool
	Close(willBeResumed bool)
	OnClose(f func(willBeResumed bool))
	ID() livekit.TrackID
	PublisherID() livekit.ParticipantID
	PublisherIdentity() livekit.ParticipantIdentity
	PublisherVersion() uint32
	SubscriberID() livekit.ParticipantID
	SubscriberIdentity() livekit.ParticipantIdentity
	Subscriber() LocalParticipant
	DownTrack() *sfu.DownTrack
	MediaTrack() MediaTrack
	RTPSender() *webrtc.RTPSender
	IsMuted() bool
	SetPublisherMuted(muted bool)
	UpdateSubscriberSettings(settings *livekit.UpdateTrackSettings)
	// selects appropriate video layer according to subscriber preferences
	UpdateVideoLayer()
	NeedsNegotiation() bool
}

type ChangeNotifier interface {
	AddObserver(key string, onChanged func())
	RemoveObserver(key string)
	HasObservers() bool
	NotifyChanged()
}

type MediaResolverResult struct {
	TrackChangedNotifier ChangeNotifier
	TrackRemovedNotifier ChangeNotifier
	Track                MediaTrack
	// is permission given to the requesting participant
	HasPermission     bool
	PublisherID       livekit.ParticipantID
	PublisherIdentity livekit.ParticipantIdentity
}

// MediaTrackResolver locates a specific media track for a subscriber
type MediaTrackResolver func(livekit.ParticipantIdentity, livekit.TrackID) MediaResolverResult

// Supervisor/operation monitor related definitions
type OperationMonitorEvent int

const (
	OperationMonitorEventPublisherPeerConnectionConnected OperationMonitorEvent = iota
	OperationMonitorEventAddPendingPublication
	OperationMonitorEventSetPublicationMute
	OperationMonitorEventSetPublishedTrack
	OperationMonitorEventClearPublishedTrack
)

func (o OperationMonitorEvent) String() string {
	switch o {
	case OperationMonitorEventPublisherPeerConnectionConnected:
		return "PUBLISHER_PEER_CONNECTION_CONNECTED"
	case OperationMonitorEventAddPendingPublication:
		return "ADD_PENDING_PUBLICATION"
	case OperationMonitorEventSetPublicationMute:
		return "SET_PUBLICATION_MUTE"
	case OperationMonitorEventSetPublishedTrack:
		return "SET_PUBLISHED_TRACK"
	case OperationMonitorEventClearPublishedTrack:
		return "CLEAR_PUBLISHED_TRACK"
	default:
		return fmt.Sprintf("%d", int(o))
	}
}

type OperationMonitorData interface{}

type OperationMonitor interface {
	PostEvent(ome OperationMonitorEvent, omd OperationMonitorData)
	Check() error
	IsIdle() bool
}
