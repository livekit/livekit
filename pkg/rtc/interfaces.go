package rtc

import (
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"

	"github.com/livekit/livekit-server/pkg/sfu"
	"github.com/livekit/livekit-server/proto/livekit"
)

type WebsocketClient interface {
	ReadMessage() (messageType int, p []byte, err error)
	WriteMessage(messageType int, data []byte) error
	WriteControl(messageType int, data []byte, deadline time.Time) error
}

type SignalConnection interface {
	ReadRequest() (*livekit.SignalRequest, error)
	WriteResponse(*livekit.SignalResponse) error
}

// use interface to make it easier to mock
type PeerConnection interface {
	OnICECandidate(f func(*webrtc.ICECandidate))
	OnICEConnectionStateChange(func(webrtc.ICEConnectionState))
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
	RemoveTrack(sender *webrtc.RTPSender) error
}

type Participant interface {
	ID() string
	Name() string
	State() livekit.ParticipantInfo_State
	ToProto() *livekit.ParticipantInfo
	Answer(sdp webrtc.SessionDescription) (answer webrtc.SessionDescription, err error)
	HandleNegotiate(sd webrtc.SessionDescription) error
	SetRemoteDescription(sdp webrtc.SessionDescription) error
	AddICECandidate(candidate webrtc.ICECandidateInit) error

	AddSubscriber(op Participant) error
	RemoveSubscriber(peerId string)
	SendJoinResponse(otherParticipants []Participant) error
	SendParticipantUpdate(participants []*livekit.ParticipantInfo) error

	Start()
	Close() error

	// callbacks
	OnOffer(func(webrtc.SessionDescription))
	OnICECandidate(func(c *webrtc.ICECandidateInit))
	OnStateChange(func(p Participant, oldState livekit.ParticipantInfo_State))
	OnTrackPublished(func(Participant, PublishedTrack))
	OnClose(func(Participant))

	// package methods
	addDownTrack(streamId string, dt *sfu.DownTrack)
	removeDownTrack(streamId string, dt *sfu.DownTrack)
	peerConnection() PeerConnection
}

// PublishedTrack is the main interface representing a track published to the room
// it's responsible for managing subscribers and forwarding data from the input track to all subscribers
type PublishedTrack interface {
	Start()
	ID() string
	Kind() livekit.TrackInfo_Type
	StreamID() string
	AddSubscriber(participant Participant) error
	RemoveSubscriber(participantId string)
	RemoveAllSubscribers()
}
