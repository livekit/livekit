package types

import (
	"time"

	"github.com/livekit/protocol/livekit"
	"github.com/pion/rtcp"
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

//counterfeiter:generate . Participant
type Participant interface {
	ID() string
	Identity() string
	State() livekit.ParticipantInfo_State
	ProtocolVersion() ProtocolVersion
	IsReady() bool
	ConnectedAt() time.Time
	ToProto() *livekit.ParticipantInfo
	RTCPChan() chan []rtcp.Packet
	SetMetadata(metadata string)
	SetPermission(permission *livekit.ParticipantPermission)
	GetResponseSink() routing.MessageSink
	SetResponseSink(sink routing.MessageSink)
	SubscriberMediaEngine() *webrtc.MediaEngine
	Negotiate()
	ICERestart() error

	AddTrack(req *livekit.AddTrackRequest)
	GetPublishedTrack(sid string) PublishedTrack
	GetPublishedTracks() []PublishedTrack
	GetSubscribedTrack(sid string) SubscribedTrack
	GetSubscribedTracks() []SubscribedTrack
	HandleOffer(sdp webrtc.SessionDescription) (answer webrtc.SessionDescription, err error)
	HandleAnswer(sdp webrtc.SessionDescription) error
	AddICECandidate(candidate webrtc.ICECandidateInit, target livekit.SignalTarget) error
	AddSubscriber(op Participant) (int, error)
	SendJoinResponse(info *livekit.Room, otherParticipants []*livekit.ParticipantInfo, iceServers []*livekit.ICEServer) error
	SendParticipantUpdate(participants []*livekit.ParticipantInfo, updatedAt time.Time) error
	SendSpeakerUpdate(speakers []*livekit.SpeakerInfo) error
	SendDataPacket(packet *livekit.DataPacket) error
	SendRoomUpdate(room *livekit.Room) error
	SendConnectionQualityUpdate(update *livekit.ConnectionQualityUpdate) error
	SetTrackMuted(trackId string, muted bool, fromAdmin bool)
	GetAudioLevel() (level uint8, active bool)
	GetConnectionQuality() livekit.ConnectionQuality
	IsSubscribedTo(identity string) bool
	// returns list of participant identities that the current participant is subscribed to
	GetSubscribedParticipants() []string

	// permissions

	CanPublish() bool
	CanSubscribe() bool
	CanPublishData() bool
	Hidden() bool
	IsRecorder() bool
	SubscriberAsPrimary() bool

	Start()
	Close() error

	// callbacks

	OnStateChange(func(p Participant, oldState livekit.ParticipantInfo_State))
	// OnTrackPublished - remote added a remoteTrack
	OnTrackPublished(func(Participant, PublishedTrack))
	// OnTrackUpdated - one of its publishedTracks changed in status
	OnTrackUpdated(callback func(Participant, PublishedTrack))
	OnMetadataUpdate(callback func(Participant))
	OnDataPacket(callback func(Participant, *livekit.DataPacket))
	OnClose(func(Participant))

	// package methods
	AddSubscribedTrack(st SubscribedTrack)
	RemoveSubscribedTrack(st SubscribedTrack)
	SubscriberPC() *webrtc.PeerConnection

	DebugInfo() map[string]interface{}
}

// PublishedTrack is the main interface representing a track published to the room
// it's responsible for managing subscribers and forwarding data from the input track to all subscribers
//counterfeiter:generate . PublishedTrack
type PublishedTrack interface {
	Start()
	ID() string
	SignalCid() string
	SdpCid() string
	Kind() livekit.TrackType
	Name() string
	IsMuted() bool
	SetMuted(muted bool)
	UpdateVideoLayers(layers []*livekit.VideoLayer)

	// subscribers
	AddSubscriber(participant Participant) error
	RemoveSubscriber(participantId string)
	IsSubscriber(subId string) bool
	RemoveAllSubscribers()
	// returns quality information that's appropriate for width & height
	GetQualityForDimension(width, height uint32) livekit.VideoQuality
	// returns number of uptracks that are publishing, registered
	NumUpTracks() (uint32, uint32)
	PublishLossPercentage() uint32
	ToProto() *livekit.TrackInfo
	Receiver() sfu.TrackReceiver

	// callbacks
	AddOnClose(func())
}

//counterfeiter:generate . SubscribedTrack
type SubscribedTrack interface {
	ID() string
	PublisherIdentity() string
	DownTrack() *sfu.DownTrack
	IsMuted() bool
	SetPublisherMuted(muted bool)
	UpdateSubscriberSettings(settings *livekit.UpdateTrackSettings)
	// selects appropriate video layer according to subscriber preferences
	UpdateVideoLayer()
	SubscribeLossPercentage() uint32
}

// interface for properties of webrtc.TrackRemote
//counterfeiter:generate . TrackRemote
type TrackRemote interface {
	SSRC() webrtc.SSRC
	StreamID() string
	Kind() webrtc.RTPCodecType
	Codec() webrtc.RTPCodecParameters
}
