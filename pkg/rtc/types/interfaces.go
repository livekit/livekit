package types

import (
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"

	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/sfu"
	"github.com/livekit/livekit-server/pkg/utils"
	"github.com/livekit/livekit-server/proto/livekit"
)

//go:generate go run github.com/maxbrunsfeld/counterfeiter/v6 -generate

//counterfeiter:generate . WebsocketClient
type WebsocketClient interface {
	ReadMessage() (messageType int, p []byte, err error)
	WriteMessage(messageType int, data []byte) error
	WriteControl(messageType int, data []byte, deadline time.Time) error
}

//counterfeiter:generate . PeerConnection
type PeerConnection interface {
	OnICECandidate(f func(*webrtc.ICECandidate))
	OnICEConnectionStateChange(func(webrtc.ICEConnectionState))
	//OnConnectionStateChange(f func(webrtc.PeerConnectionState))
	OnTrack(f func(*webrtc.TrackRemote, *webrtc.RTPReceiver))
	OnDataChannel(func(d *webrtc.DataChannel))
	OnNegotiationNeeded(f func())
	Close() error
	SetRemoteDescription(desc webrtc.SessionDescription) error
	SetLocalDescription(desc webrtc.SessionDescription) error
	CreateOffer(options *webrtc.OfferOptions) (webrtc.SessionDescription, error)
	CreateAnswer(options *webrtc.AnswerOptions) (webrtc.SessionDescription, error)
	AddICECandidate(candidate webrtc.ICECandidateInit) error
	WriteRTCP(pkts []rtcp.Packet) error
	// used by datatrack
	CreateDataChannel(label string, options *webrtc.DataChannelInit) (*webrtc.DataChannel, error)
	// used by mediatrack
	AddTransceiverFromTrack(track webrtc.TrackLocal, init ...webrtc.RtpTransceiverInit) (*webrtc.RTPTransceiver, error)
	ConnectionState() webrtc.PeerConnectionState
	SignalingState() webrtc.SignalingState
	RemoveTrack(sender *webrtc.RTPSender) error
}

//counterfeiter:generate . Participant
type Participant interface {
	ID() string
	Identity() string
	State() livekit.ParticipantInfo_State
	IsReady() bool
	ToProto() *livekit.ParticipantInfo
	RTCPChan() *utils.CalmChannel
	GetResponseSink() routing.MessageSink
	SetResponseSink(sink routing.MessageSink)

	AddTrack(clientId, name string, trackType livekit.TrackType)
	Answer(sdp webrtc.SessionDescription) (answer webrtc.SessionDescription, err error)
	HandleAnswer(sdp webrtc.SessionDescription) error
	HandleClientNegotiation()
	AddICECandidate(candidate webrtc.ICECandidateInit) error
	AddSubscriber(op Participant) error
	RemoveSubscriber(peerId string)
	SendJoinResponse(info *livekit.Room, otherParticipants []Participant) error
	SendParticipantUpdate(participants []*livekit.ParticipantInfo) error
	SetTrackMuted(trackId string, muted bool)

	Start()
	Close() error

	// callbacks
	// OnICECandidate - ice candidate discovered for local peer
	OnICECandidate(func(c *webrtc.ICECandidateInit))
	OnStateChange(func(p Participant, oldState livekit.ParticipantInfo_State))
	// OnTrackPublished - remote added a remoteTrack
	OnTrackPublished(func(Participant, PublishedTrack))
	// OnTrackUpdated - one of its publishedTracks changed in status
	OnTrackUpdated(callback func(Participant, PublishedTrack))
	OnClose(func(Participant))

	// package methods
	AddDownTrack(streamId string, dt *sfu.DownTrack)
	RemoveDownTrack(streamId string, dt *sfu.DownTrack)
	PeerConnection() PeerConnection
}

// PublishedTrack is the main interface representing a track published to the room
// it's responsible for managing subscribers and forwarding data from the input track to all subscribers
//counterfeiter:generate . PublishedTrack
type PublishedTrack interface {
	Start()
	ID() string
	Kind() livekit.TrackType
	Name() string
	IsMuted() bool
	AddSubscriber(participant Participant) error
	RemoveSubscriber(participantId string)
	RemoveAllSubscribers()

	// callbacks
	OnClose(func())
}

//counterfeiter:generate . Receiver
type Receiver interface {
	RTPChan() <-chan rtp.Packet
	GetBufferedPacket(pktBuf []byte, sn uint16, snOffset uint16) (rtp.Packet, error)
}

// DownTrack publishes data to a target participant
// using this interface to make testing more practical
//counterfeiter:generate . DownTrack
type DownTrack interface {
	ID() string
	WriteRTP(p rtp.Packet) error
	IsBound() bool
	Close()
	OnCloseHandler(fn func())
	OnBind(fn func())
	SSRC() uint32
	LastSSRC() uint32
	SnOffset() uint16
	TsOffset() uint32
	GetNACKSeqNo(seqNo []uint16) []uint16
	CreateSourceDescriptionChunks() []rtcp.SourceDescriptionChunk
}

// interface for properties of webrtc.TrackRemote
//counterfeiter:generate . TrackRemote
type TrackRemote interface {
	SSRC() webrtc.SSRC
	StreamID() string
	Kind() webrtc.RTPCodecType
	Codec() webrtc.RTPCodecParameters
}
