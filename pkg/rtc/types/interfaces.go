package types

import (
	"time"

	"github.com/livekit/protocol/livekit"
	"github.com/pion/webrtc/v3"

	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/sfu"
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

type MigrateState int32

const (
	MigrateStateInit MigrateState = iota
	MigrateStateSync
	MigrateStateComplete
)

//counterfeiter:generate . Participant
type Participant interface {
	ID() livekit.ParticipantID
	Identity() livekit.ParticipantIdentity

	ToProto() *livekit.ParticipantInfo

	SetMetadata(metadata string)

	GetPublishedTrack(sid livekit.TrackID) PublishedTrack
	GetPublishedTracks() []PublishedTrack

	AddSubscriber(op Participant, params AddSubscriberParams) (int, error)
	RemoveSubscriber(op Participant, trackID livekit.TrackID)

	// permissions
	Hidden() bool

	Start()
	Close() error

	// callbacks
	// OnTrackPublished - remote added a remoteTrack
	OnTrackPublished(func(Participant, PublishedTrack))

	UpdateSubscriptionPermissions(permissions *livekit.UpdateSubscriptionPermissions, resolver func(participantID livekit.ParticipantID) Participant) error
	UpdateVideoLayers(updateVideoLayers *livekit.UpdateVideoLayers) error

	DebugInfo() map[string]interface{}
}

//counterfeiter:generate . LocalParticipant
type LocalParticipant interface {
	Participant

	ProtocolVersion() ProtocolVersion

	ConnectedAt() time.Time

	State() livekit.ParticipantInfo_State
	IsReady() bool

	IsRecorder() bool

	SubscriberAsPrimary() bool

	GetResponseSink() routing.MessageSink
	SetResponseSink(sink routing.MessageSink)

	// permissions
	SetPermission(permission *livekit.ParticipantPermission)
	CanPublish() bool
	CanSubscribe() bool
	CanPublishData() bool

	AddICECandidate(candidate webrtc.ICECandidateInit, target livekit.SignalTarget) error

	HandleOffer(sdp webrtc.SessionDescription) (answer webrtc.SessionDescription, err error)

	AddTrack(req *livekit.AddTrackRequest)
	SetTrackMuted(trackID livekit.TrackID, muted bool, fromAdmin bool)

	SubscriberMediaEngine() *webrtc.MediaEngine
	SubscriberPC() *webrtc.PeerConnection
	HandleAnswer(sdp webrtc.SessionDescription) error
	Negotiate()
	ICERestart() error
	AddSubscribedTrack(st SubscribedTrack)
	RemoveSubscribedTrack(st SubscribedTrack)
	GetSubscribedTrack(sid livekit.TrackID) SubscribedTrack
	GetSubscribedTracks() []SubscribedTrack

	// returns list of participant identities that the current participant is subscribed to
	GetSubscribedParticipants() []livekit.ParticipantID

	GetAudioLevel() (level uint8, active bool)
	GetConnectionQuality() *livekit.ConnectionQualityInfo

	// server sent messages
	SendJoinResponse(info *livekit.Room, otherParticipants []*livekit.ParticipantInfo, iceServers []*livekit.ICEServer) error
	SendParticipantUpdate(participants []*livekit.ParticipantInfo, updatedAt time.Time) error
	SendSpeakerUpdate(speakers []*livekit.SpeakerInfo) error
	SendDataPacket(packet *livekit.DataPacket) error
	SendRoomUpdate(room *livekit.Room) error
	SendConnectionQualityUpdate(update *livekit.ConnectionQualityUpdate) error
	SubscriptionPermissionUpdate(publisherID livekit.ParticipantID, trackID livekit.TrackID, allowed bool)

	// callbacks
	OnStateChange(func(p Participant, oldState livekit.ParticipantInfo_State))
	// OnTrackUpdated - one of its publishedTracks changed in status
	OnTrackUpdated(callback func(Participant, PublishedTrack))
	OnMetadataUpdate(callback func(Participant))
	OnDataPacket(callback func(Participant, *livekit.DataPacket))
	OnClose(_callback func(Participant, map[livekit.TrackID]livekit.ParticipantID))

	// updates from remotes
	UpdateSubscribedQuality(nodeID string, trackID livekit.TrackID, maxQuality livekit.VideoQuality) error
	UpdateMediaLoss(nodeID string, trackID livekit.TrackID, fractionalLoss uint32) error

	// session migration
	SetMigrateState(s MigrateState)
	MigrateState() MigrateState
	AddMigratedTrack(cid string, ti *livekit.TrackInfo)
	SetPreviousAnswer(previous *webrtc.SessionDescription)
}

// Room is a container of participants, and can provide room level actions
//counterfeiter:generate . Room
type Room interface {
	Name() livekit.RoomName
	UpdateSubscriptions(participant Participant, trackIDs []livekit.TrackID, participantTracks []*livekit.ParticipantTracks, subscribe bool) error
	UpdateSubscriptionPermissions(participant Participant, permissions *livekit.UpdateSubscriptionPermissions) error
	SyncState(participant Participant, state *livekit.SyncState) error

	UpdateVideoLayers(participant Participant, updateVideoLayers *livekit.UpdateVideoLayers) error
}

//counterfeiter:generate . LocalMediaTrack
type LocalMediaTrack interface {
	NotifySubscriberNodeMediaLoss(nodeID string, fractionalLoss uint8)
}

// MediaTrack represents a media track
//counterfeiter:generate . MediaTrack
type MediaTrack interface {
	LocalMediaTrack

	ID() livekit.TrackID
	Kind() livekit.TrackType
	Name() string
	IsMuted() bool
	SetMuted(muted bool)
	UpdateVideoLayers(layers []*livekit.VideoLayer)
	Source() livekit.TrackSource
	IsSimulcast() bool

	PublisherID() livekit.ParticipantID
	PublisherIdentity() livekit.ParticipantIdentity

	ToProto() *livekit.TrackInfo

	// subscribers
	AddSubscriber(participant Participant) error
	RemoveSubscriber(participantID livekit.ParticipantID)
	IsSubscriber(subID livekit.ParticipantID) bool
	GetAllSubscriberIDs() []livekit.ParticipantID
	RemoveAllSubscribers()
	RevokeDisallowedSubscribers(allowedSubscriberIDs []livekit.ParticipantID) []livekit.ParticipantID

	// returns quality information that's appropriate for width & height
	GetQualityForDimension(width, height uint32) livekit.VideoQuality

	NotifySubscriberMaxQuality(subscriberID livekit.ParticipantID, quality livekit.VideoQuality)
	NotifySubscriberNodeMaxQuality(nodeID string, quality livekit.VideoQuality)
}

//counterfeiter:generate . LocalPublishedTrack
type LocalPublishedTrack interface {
	SignalCid() string
	SdpCid() string

	GetAudioLevel() (level uint8, active bool)
	GetConnectionScore() float64
}

// PublishedTrack is the main interface representing a track published to the room
// it's responsible for managing subscribers and forwarding data from the input track to all subscribers
//counterfeiter:generate . PublishedTrack
type PublishedTrack interface {
	MediaTrack

	ToProto() *livekit.TrackInfo

	Receiver() sfu.TrackReceiver

	UpdateVideoLayers(layers []*livekit.VideoLayer)

	OnSubscribedMaxQualityChange(f func(trackID livekit.TrackID, subscribedQualities []*livekit.SubscribedQuality, maxQuality livekit.VideoQuality) error)

	// callbacks
	AddOnClose(func())

	LocalPublishedTrack
}

//counterfeiter:generate . SubscribedTrack
type SubscribedTrack interface {
	OnBind(f func())
	ID() livekit.TrackID
	PublisherID() livekit.ParticipantID
	PublisherIdentity() livekit.ParticipantIdentity
	DownTrack() *sfu.DownTrack
	MediaTrack() MediaTrack
	IsMuted() bool
	SetPublisherMuted(muted bool)
	UpdateSubscriberSettings(settings *livekit.UpdateTrackSettings)
	// selects appropriate video layer according to subscriber preferences
	UpdateVideoLayer()
}

// interface for properties of webrtc.TrackRemote
//counterfeiter:generate . TrackRemote
type TrackRemote interface {
	SSRC() webrtc.SSRC
	StreamID() string
	Kind() webrtc.RTPCodecType
	Codec() webrtc.RTPCodecParameters
}
