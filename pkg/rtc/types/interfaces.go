// Copyright 2023 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package types

import (
	"fmt"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/observability/roomobs"
	"github.com/livekit/protocol/utils"

	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/sfu"
	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/sfu/mime"
	"github.com/livekit/livekit-server/pkg/sfu/pacer"
	"github.com/livekit/livekit-server/pkg/telemetry"

	"google.golang.org/protobuf/proto"
)

//go:generate go run github.com/maxbrunsfeld/counterfeiter/v6 -generate

//counterfeiter:generate . WebsocketClient
type WebsocketClient interface {
	ReadMessage() (messageType int, p []byte, err error)
	WriteMessage(messageType int, data []byte) error
	WriteControl(messageType int, data []byte, deadline time.Time) error
	SetReadDeadline(deadline time.Time) error
	Close() error
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
	CodecMime mime.MimeType
	Quality   livekit.VideoQuality
}

// ---------------------------------------------

type ParticipantCloseReason int

const (
	ParticipantCloseReasonNone ParticipantCloseReason = iota
	ParticipantCloseReasonClientRequestLeave
	ParticipantCloseReasonRoomManagerStop
	ParticipantCloseReasonVerifyFailed
	ParticipantCloseReasonJoinFailed
	ParticipantCloseReasonJoinTimeout
	ParticipantCloseReasonMessageBusFailed
	ParticipantCloseReasonPeerConnectionDisconnected
	ParticipantCloseReasonDuplicateIdentity
	ParticipantCloseReasonMigrationComplete
	ParticipantCloseReasonStale
	ParticipantCloseReasonServiceRequestRemoveParticipant
	ParticipantCloseReasonServiceRequestDeleteRoom
	ParticipantCloseReasonSimulateMigration
	ParticipantCloseReasonSimulateNodeFailure
	ParticipantCloseReasonSimulateServerLeave
	ParticipantCloseReasonSimulateLeaveRequest
	ParticipantCloseReasonNegotiateFailed
	ParticipantCloseReasonMigrationRequested
	ParticipantCloseReasonPublicationError
	ParticipantCloseReasonSubscriptionError
	ParticipantCloseReasonDataChannelError
	ParticipantCloseReasonMigrateCodecMismatch
	ParticipantCloseReasonSignalSourceClose
	ParticipantCloseReasonRoomClosed
	ParticipantCloseReasonUserUnavailable
	ParticipantCloseReasonUserRejected
	ParticipantCloseReasonMoveFailed
)

func (p ParticipantCloseReason) String() string {
	switch p {
	case ParticipantCloseReasonNone:
		return "NONE"
	case ParticipantCloseReasonClientRequestLeave:
		return "CLIENT_REQUEST_LEAVE"
	case ParticipantCloseReasonRoomManagerStop:
		return "ROOM_MANAGER_STOP"
	case ParticipantCloseReasonVerifyFailed:
		return "VERIFY_FAILED"
	case ParticipantCloseReasonJoinFailed:
		return "JOIN_FAILED"
	case ParticipantCloseReasonJoinTimeout:
		return "JOIN_TIMEOUT"
	case ParticipantCloseReasonMessageBusFailed:
		return "MESSAGE_BUS_FAILED"
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
	case ParticipantCloseReasonSimulateLeaveRequest:
		return "SIMULATE_LEAVE_REQUEST"
	case ParticipantCloseReasonNegotiateFailed:
		return "NEGOTIATE_FAILED"
	case ParticipantCloseReasonMigrationRequested:
		return "MIGRATION_REQUESTED"
	case ParticipantCloseReasonPublicationError:
		return "PUBLICATION_ERROR"
	case ParticipantCloseReasonSubscriptionError:
		return "SUBSCRIPTION_ERROR"
	case ParticipantCloseReasonDataChannelError:
		return "DATA_CHANNEL_ERROR"
	case ParticipantCloseReasonMigrateCodecMismatch:
		return "MIGRATE_CODEC_MISMATCH"
	case ParticipantCloseReasonSignalSourceClose:
		return "SIGNAL_SOURCE_CLOSE"
	case ParticipantCloseReasonRoomClosed:
		return "ROOM_CLOSED"
	case ParticipantCloseReasonUserUnavailable:
		return "USER_UNAVAILABLE"
	case ParticipantCloseReasonUserRejected:
		return "USER_REJECTED"
	case ParticipantCloseReasonMoveFailed:
		return "MOVE_FAILED"
	default:
		return fmt.Sprintf("%d", int(p))
	}
}

func (p ParticipantCloseReason) ToDisconnectReason() livekit.DisconnectReason {
	switch p {
	case ParticipantCloseReasonClientRequestLeave, ParticipantCloseReasonSimulateLeaveRequest:
		return livekit.DisconnectReason_CLIENT_INITIATED
	case ParticipantCloseReasonRoomManagerStop:
		return livekit.DisconnectReason_SERVER_SHUTDOWN
	case ParticipantCloseReasonVerifyFailed, ParticipantCloseReasonJoinFailed, ParticipantCloseReasonJoinTimeout, ParticipantCloseReasonMessageBusFailed:
		// expected to be connected but is not
		return livekit.DisconnectReason_JOIN_FAILURE
	case ParticipantCloseReasonPeerConnectionDisconnected:
		return livekit.DisconnectReason_CONNECTION_TIMEOUT
	case ParticipantCloseReasonDuplicateIdentity, ParticipantCloseReasonStale:
		return livekit.DisconnectReason_DUPLICATE_IDENTITY
	case ParticipantCloseReasonMigrationRequested, ParticipantCloseReasonMigrationComplete, ParticipantCloseReasonSimulateMigration:
		return livekit.DisconnectReason_MIGRATION
	case ParticipantCloseReasonServiceRequestRemoveParticipant:
		return livekit.DisconnectReason_PARTICIPANT_REMOVED
	case ParticipantCloseReasonServiceRequestDeleteRoom:
		return livekit.DisconnectReason_ROOM_DELETED
	case ParticipantCloseReasonSimulateNodeFailure, ParticipantCloseReasonSimulateServerLeave:
		return livekit.DisconnectReason_SERVER_SHUTDOWN
	case ParticipantCloseReasonNegotiateFailed, ParticipantCloseReasonPublicationError, ParticipantCloseReasonSubscriptionError,
		ParticipantCloseReasonDataChannelError, ParticipantCloseReasonMigrateCodecMismatch, ParticipantCloseReasonMoveFailed:
		return livekit.DisconnectReason_STATE_MISMATCH
	case ParticipantCloseReasonSignalSourceClose:
		return livekit.DisconnectReason_SIGNAL_CLOSE
	case ParticipantCloseReasonRoomClosed:
		return livekit.DisconnectReason_ROOM_CLOSED
	case ParticipantCloseReasonUserUnavailable:
		return livekit.DisconnectReason_USER_UNAVAILABLE
	case ParticipantCloseReasonUserRejected:
		return livekit.DisconnectReason_USER_REJECTED
	default:
		// the other types will map to unknown reason
		return livekit.DisconnectReason_UNKNOWN_REASON
	}
}

// ---------------------------------------------

type SignallingCloseReason int

const (
	SignallingCloseReasonUnknown SignallingCloseReason = iota
	SignallingCloseReasonMigration
	SignallingCloseReasonResume
	SignallingCloseReasonTransportFailure
	SignallingCloseReasonFullReconnectPublicationError
	SignallingCloseReasonFullReconnectSubscriptionError
	SignallingCloseReasonFullReconnectDataChannelError
	SignallingCloseReasonFullReconnectNegotiateFailed
	SignallingCloseReasonParticipantClose
	SignallingCloseReasonDisconnectOnResume
	SignallingCloseReasonDisconnectOnResumeNoMessages
)

func (s SignallingCloseReason) String() string {
	switch s {
	case SignallingCloseReasonUnknown:
		return "UNKNOWN"
	case SignallingCloseReasonMigration:
		return "MIGRATION"
	case SignallingCloseReasonResume:
		return "RESUME"
	case SignallingCloseReasonTransportFailure:
		return "TRANSPORT_FAILURE"
	case SignallingCloseReasonFullReconnectPublicationError:
		return "FULL_RECONNECT_PUBLICATION_ERROR"
	case SignallingCloseReasonFullReconnectSubscriptionError:
		return "FULL_RECONNECT_SUBSCRIPTION_ERROR"
	case SignallingCloseReasonFullReconnectDataChannelError:
		return "FULL_RECONNECT_DATA_CHANNEL_ERROR"
	case SignallingCloseReasonFullReconnectNegotiateFailed:
		return "FULL_RECONNECT_NEGOTIATE_FAILED"
	case SignallingCloseReasonParticipantClose:
		return "PARTICIPANT_CLOSE"
	case SignallingCloseReasonDisconnectOnResume:
		return "DISCONNECT_ON_RESUME"
	case SignallingCloseReasonDisconnectOnResumeNoMessages:
		return "DISCONNECT_ON_RESUME_NO_MESSAGES"
	default:
		return fmt.Sprintf("%d", int(s))
	}
}

// ---------------------------------------------
const (
	ParticipantCloseKeyNormal = "normal"
	ParticipantCloseKeyWHIP   = "whip"
)

// ---------------------------------------------

//counterfeiter:generate . Participant
type Participant interface {
	ID() livekit.ParticipantID
	Identity() livekit.ParticipantIdentity
	State() livekit.ParticipantInfo_State
	ConnectedAt() time.Time
	CloseReason() ParticipantCloseReason
	Kind() livekit.ParticipantInfo_Kind
	IsRecorder() bool
	IsDependent() bool
	IsAgent() bool

	CanSkipBroadcast() bool
	Version() utils.TimedVersion
	ToProto() *livekit.ParticipantInfo

	IsPublisher() bool
	GetPublishedTrack(trackID livekit.TrackID) MediaTrack
	GetPublishedTracks() []MediaTrack
	RemovePublishedTrack(track MediaTrack, isExpectedToResume bool)

	GetAudioLevel() (smoothedLevel float64, active bool)

	// HasPermission checks permission of the subscriber by identity. Returns true if subscriber is allowed to subscribe
	// to the track with trackID
	HasPermission(trackID livekit.TrackID, subIdentity livekit.ParticipantIdentity) bool

	// permissions
	Hidden() bool

	Close(sendLeave bool, reason ParticipantCloseReason, isExpectedToResume bool) error

	SubscriptionPermission() (*livekit.SubscriptionPermission, utils.TimedVersion)

	// updates from remotes
	UpdateSubscriptionPermission(
		subscriptionPermission *livekit.SubscriptionPermission,
		timedVersion utils.TimedVersion,
		resolverBySid func(participantID livekit.ParticipantID) LocalParticipant,
	) error

	DebugInfo() map[string]interface{}

	OnMetrics(callback func(Participant, *livekit.DataPacket))
}

// -------------------------------------------------------

type AddTrackParams struct {
	Stereo bool
	Red    bool
}

type MoveToRoomParams struct {
	RoomName      livekit.RoomName
	ParticipantID livekit.ParticipantID
	Helper        LocalParticipantHelper
}

type DataMessageCache struct {
	Data           []byte
	SenderID       livekit.ParticipantID
	Seq            uint32
	DestIdentities []livekit.ParticipantIdentity
}

//counterfeiter:generate . LocalParticipantHelper
type LocalParticipantHelper interface {
	ResolveMediaTrack(LocalParticipant, livekit.TrackID) MediaResolverResult
	GetParticipantInfo(pID livekit.ParticipantID) *livekit.ParticipantInfo
	GetRegionSettings(ip string) *livekit.RegionSettings
	GetSubscriberForwarderState(p LocalParticipant) (map[livekit.TrackID]*livekit.RTPForwarderState, error)
	ShouldRegressCodec() bool
	GetCachedReliableDataMessage(seqs map[livekit.ParticipantID]uint32) []*DataMessageCache
}

//counterfeiter:generate . LocalParticipant
type LocalParticipant interface {
	Participant

	TelemetryGuard() *telemetry.ReferenceGuard
	ToProtoWithVersion() (*livekit.ParticipantInfo, utils.TimedVersion)

	// getters
	GetCountry() string
	GetTrailer() []byte
	GetLogger() logger.Logger
	GetLoggerResolver() logger.DeferredFieldResolver
	GetReporter() roomobs.ParticipantSessionReporter
	GetReporterResolver() roomobs.ParticipantReporterResolver
	GetAdaptiveStream() bool
	ProtocolVersion() ProtocolVersion
	SupportsSyncStreamID() bool
	SupportsTransceiverReuse() bool
	IsUsingSinglePeerConnection() bool
	IsClosed() bool
	IsReady() bool
	IsDisconnected() bool
	Disconnected() <-chan struct{}
	IsIdle() bool
	SubscriberAsPrimary() bool
	GetClientInfo() *livekit.ClientInfo
	GetClientConfiguration() *livekit.ClientConfiguration
	GetBufferFactory() *buffer.Factory
	GetPlayoutDelayConfig() *livekit.PlayoutDelay
	GetPendingTrack(trackID livekit.TrackID) *livekit.TrackInfo
	GetICEConnectionInfo() []*ICEConnectionInfo
	HasConnected() bool
	GetEnabledPublishCodecs() []*livekit.Codec
	GetPublisherICESessionUfrag() (string, error)
	SupportsMoving() error
	GetLastReliableSequence(migrateOut bool) uint32

	SetResponseSink(sink routing.MessageSink)
	GetResponseSink() routing.MessageSink
	CloseSignalConnection(reason SignallingCloseReason)
	UpdateLastSeenSignal()
	SetSignalSourceValid(valid bool)
	HandleSignalSourceClose()

	// updates
	UpdateMetadata(update *livekit.UpdateParticipantMetadata, fromAdmin bool) error
	SetName(name string)
	SetMetadata(metadata string)
	SetAttributes(attributes map[string]string)
	UpdateAudioTrack(update *livekit.UpdateLocalAudioTrack) error
	UpdateVideoTrack(update *livekit.UpdateLocalVideoTrack) error

	// permissions
	ClaimGrants() *auth.ClaimGrants
	SetPermission(permission *livekit.ParticipantPermission) bool
	CanPublish() bool
	CanPublishSource(source livekit.TrackSource) bool
	CanSubscribe() bool
	CanPublishData() bool

	// PeerConnection
	HandleICETrickle(trickleRequest *livekit.TrickleRequest)
	HandleOffer(sd *livekit.SessionDescription) error
	GetAnswer() (webrtc.SessionDescription, uint32, error)
	HandleICETrickleSDPFragment(sdpFragment string) error
	HandleICERestartSDPFragment(sdpFragment string) (string, error)
	AddTrack(req *livekit.AddTrackRequest)
	SetTrackMuted(mute *livekit.MuteTrackRequest, fromAdmin bool) *livekit.TrackInfo

	HandleAnswer(sd *livekit.SessionDescription)
	Negotiate(force bool)
	ICERestart(iceConfig *livekit.ICEConfig)
	AddTrackLocal(trackLocal webrtc.TrackLocal, params AddTrackParams) (*webrtc.RTPSender, *webrtc.RTPTransceiver, error)
	AddTransceiverFromTrackLocal(trackLocal webrtc.TrackLocal, params AddTrackParams) (*webrtc.RTPSender, *webrtc.RTPTransceiver, error)
	RemoveTrackLocal(sender *webrtc.RTPSender) error

	WriteSubscriberRTCP(pkts []rtcp.Packet) error

	// subscriptions
	SubscribeToTrack(trackID livekit.TrackID, isSync bool)
	UnsubscribeFromTrack(trackID livekit.TrackID)
	UpdateSubscribedTrackSettings(trackID livekit.TrackID, settings *livekit.UpdateTrackSettings)
	GetSubscribedTracks() []SubscribedTrack
	IsTrackNameSubscribed(publisherIdentity livekit.ParticipantIdentity, trackName string) bool
	Verify() bool
	VerifySubscribeParticipantInfo(pID livekit.ParticipantID, version uint32)
	// WaitUntilSubscribed waits until all subscriptions have been settled, or if the timeout
	// has been reached. If the timeout expires, it will return an error.
	WaitUntilSubscribed(timeout time.Duration) error
	StopAndGetSubscribedTracksForwarderState() map[livekit.TrackID]*livekit.RTPForwarderState
	SupportsCodecChange() bool

	// returns list of participant identities that the current participant is subscribed to
	GetSubscribedParticipants() []livekit.ParticipantID
	IsSubscribedTo(sid livekit.ParticipantID) bool

	GetConnectionQuality() *livekit.ConnectionQualityInfo

	// server sent messages
	SendJoinResponse(joinResponse *livekit.JoinResponse) error
	SendParticipantUpdate(participants []*livekit.ParticipantInfo) error
	SendSpeakerUpdate(speakers []*livekit.SpeakerInfo, force bool) error
	SendDataMessage(kind livekit.DataPacket_Kind, data []byte, senderID livekit.ParticipantID, seq uint32) error
	SendDataMessageUnlabeled(data []byte, useRaw bool, sender livekit.ParticipantIdentity) error
	SendRoomUpdate(room *livekit.Room) error
	SendConnectionQualityUpdate(update *livekit.ConnectionQualityUpdate) error
	SendSubscriptionPermissionUpdate(publisherID livekit.ParticipantID, trackID livekit.TrackID, allowed bool) error
	SendRefreshToken(token string) error
	// RAJA-REMOVE SendRequestResponse(requestResponse *livekit.RequestResponse) error
	HandleReconnectAndSendResponse(reconnectReason livekit.ReconnectReason, reconnectResponse *livekit.ReconnectResponse) error
	IssueFullReconnect(reason ParticipantCloseReason)
	SendRoomMovedResponse(moved *livekit.RoomMovedResponse) error

	// callbacks
	OnStateChange(func(p LocalParticipant))
	OnSubscriberReady(callback func(LocalParticipant))
	OnMigrateStateChange(func(p LocalParticipant, migrateState MigrateState))
	// OnTrackPublished - remote added a track
	OnTrackPublished(func(LocalParticipant, MediaTrack))
	// OnTrackUpdated - one of its publishedTracks changed in status
	OnTrackUpdated(callback func(LocalParticipant, MediaTrack))
	// OnTrackUnpublished - a track was unpublished
	OnTrackUnpublished(callback func(LocalParticipant, MediaTrack))
	// OnParticipantUpdate - metadata or permission is updated
	OnParticipantUpdate(callback func(LocalParticipant))
	OnDataPacket(callback func(LocalParticipant, livekit.DataPacket_Kind, *livekit.DataPacket))
	OnDataMessage(callback func(LocalParticipant, []byte))
	OnSubscribeStatusChanged(fn func(publisherID livekit.ParticipantID, subscribed bool))
	AddOnClose(key string, callback func(LocalParticipant))
	OnClaimsChanged(callback func(LocalParticipant))
	OnUpdateSubscriptions(func(
		LocalParticipant,
		[]livekit.TrackID,
		[]*livekit.ParticipantTracks,
		bool,
	))
	OnUpdateSubscriptionPermission(func(LocalParticipant, *livekit.SubscriptionPermission) error)
	OnSyncState(func(LocalParticipant, *livekit.SyncState) error)
	OnSimulateScenario(func(LocalParticipant, *livekit.SimulateScenario) error)
	OnLeave(func(LocalParticipant, ParticipantCloseReason))

	HandleReceiverReport(dt *sfu.DownTrack, report *rtcp.ReceiverReport)

	// session migration
	MaybeStartMigration(force bool, onStart func()) bool
	NotifyMigration()
	SetMigrateState(s MigrateState)
	MigrateState() MigrateState
	SetMigrateInfo(
		previousOffer *webrtc.SessionDescription,
		previousAnswer *webrtc.SessionDescription,
		mediaTracks []*livekit.TrackPublishedResponse,
		dataChannels []*livekit.DataChannelInfo,
		dataChannelReceiveState []*livekit.DataChannelReceiveState,
	)
	IsReconnect() bool
	MoveToRoom(params MoveToRoomParams)

	UpdateMediaRTT(rtt uint32)
	UpdateSignalingRTT(rtt uint32)

	CacheDownTrack(trackID livekit.TrackID, rtpTransceiver *webrtc.RTPTransceiver, downTrackState sfu.DownTrackState)
	UncacheDownTrack(rtpTransceiver *webrtc.RTPTransceiver)
	GetCachedDownTrack(trackID livekit.TrackID) (*webrtc.RTPTransceiver, sfu.DownTrackState)

	SetICEConfig(iceConfig *livekit.ICEConfig)
	GetICEConfig() *livekit.ICEConfig
	OnICEConfigChanged(callback func(participant LocalParticipant, iceConfig *livekit.ICEConfig))

	UpdateSubscribedQuality(nodeID livekit.NodeID, trackID livekit.TrackID, maxQualities []SubscribedCodecQuality) error
	UpdateSubscribedAudioCodecs(nodeID livekit.NodeID, trackID livekit.TrackID, codecs []*livekit.SubscribedAudioCodec) error
	UpdateMediaLoss(nodeID livekit.NodeID, trackID livekit.TrackID, fractionalLoss uint32) error

	// down stream bandwidth management
	SetSubscriberAllowPause(allowPause bool)
	SetSubscriberChannelCapacity(channelCapacity int64)

	GetPacer() pacer.Pacer

	GetDisableSenderReportPassThrough() bool

	HandleMetrics(senderParticipantID livekit.ParticipantID, batch *livekit.MetricsBatch) error
	HandleUpdateSubscriptions(
		[]livekit.TrackID,
		[]*livekit.ParticipantTracks,
		bool,
	)
	HandleUpdateSubscriptionPermission(*livekit.SubscriptionPermission) error
	HandleSyncState(*livekit.SyncState) error
	HandleSimulateScenario(*livekit.SimulateScenario) error
	HandleLeaveRequest(reason ParticipantCloseReason)

	HandleSignalMessage(msg proto.Message) error

	PerformRpc(req *livekit.PerformRpcRequest, resultCh chan string, errorCh chan error)
}

// Room is a container of participants, and can provide room-level actions
//
//counterfeiter:generate . Room
type Room interface {
	Name() livekit.RoomName
	ID() livekit.RoomID
	RemoveParticipant(identity livekit.ParticipantIdentity, pID livekit.ParticipantID, reason ParticipantCloseReason)
	UpdateSubscriptions(
		participant LocalParticipant,
		trackIDs []livekit.TrackID,
		participantTracks []*livekit.ParticipantTracks,
		subscribe bool,
	)
	ResolveMediaTrackForSubscriber(sub LocalParticipant, trackID livekit.TrackID) MediaResolverResult
	GetLocalParticipants() []LocalParticipant
	IsDataMessageUserPacketDuplicate(ip *livekit.UserPacket) bool
}

// MediaTrack represents a media track
//
//counterfeiter:generate . MediaTrack
type MediaTrack interface {
	ID() livekit.TrackID
	Kind() livekit.TrackType
	Name() string
	Source() livekit.TrackSource
	Stream() string

	UpdateTrackInfo(ti *livekit.TrackInfo)
	UpdateAudioTrack(update *livekit.UpdateLocalAudioTrack)
	UpdateVideoTrack(update *livekit.UpdateLocalVideoTrack)
	ToProto() *livekit.TrackInfo

	PublisherID() livekit.ParticipantID
	PublisherIdentity() livekit.ParticipantIdentity
	PublisherVersion() uint32
	Logger() logger.Logger

	IsMuted() bool
	SetMuted(muted bool)

	GetAudioLevel() (level float64, active bool)

	Close(isExpectedToResume bool)
	IsOpen() bool

	// callbacks
	AddOnClose(func(isExpectedToResume bool))

	// subscribers
	AddSubscriber(participant LocalParticipant) (SubscribedTrack, error)
	RemoveSubscriber(participantID livekit.ParticipantID, isExpectedToResume bool)
	IsSubscriber(subID livekit.ParticipantID) bool
	RevokeDisallowedSubscribers(allowedSubscriberIdentities []livekit.ParticipantIdentity) []livekit.ParticipantIdentity
	GetAllSubscribers() []livekit.ParticipantID
	GetNumSubscribers() int
	OnTrackSubscribed()

	// returns quality information that's appropriate for width & height
	GetQualityForDimension(mimeType mime.MimeType, width, height uint32) livekit.VideoQuality

	// returns temporal layer that's appropriate for fps
	GetTemporalLayerForSpatialFps(mimeType mime.MimeType, spatial int32, fps uint32) int32

	Receivers() []sfu.TrackReceiver
	ClearAllReceivers(isExpectedToResume bool)

	IsEncrypted() bool
}

//counterfeiter:generate . LocalMediaTrack
type LocalMediaTrack interface {
	MediaTrack

	Restart()

	HasSignalCid(cid string) bool
	HasSdpCid(cid string) bool

	GetConnectionScoreAndQuality() (float32, livekit.ConnectionQuality)
	GetTrackStats() *livekit.RTPStats

	SetRTT(rtt uint32)

	NotifySubscriberNodeMaxQuality(nodeID livekit.NodeID, qualities []SubscribedCodecQuality)
	NotifySubscriptionNode(nodeID livekit.NodeID, codecs []*livekit.SubscribedAudioCodec)
	ClearSubscriberNodes()
	NotifySubscriberNodeMediaLoss(nodeID livekit.NodeID, fractionalLoss uint8)
}

//counterfeiter:generate . SubscribedTrack
type SubscribedTrack interface {
	AddOnBind(f func(error))
	IsBound() bool
	Close(isExpectedToResume bool)
	OnClose(f func(isExpectedToResume bool))
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
	UpdateSubscriberSettings(settings *livekit.UpdateTrackSettings, isImmediate bool)
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
type MediaTrackResolver func(LocalParticipant, livekit.TrackID) MediaResolverResult

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
