package transport

import (
	"errors"

	"github.com/pion/webrtc/v3"

	"github.com/livekit/livekit-server/pkg/sfu/streamallocator"
	"github.com/livekit/protocol/livekit"
)

//go:generate go run github.com/maxbrunsfeld/counterfeiter/v6 -generate

var (
	ErrNoICECandidateHandler = errors.New("no ICE candidate handler")
	ErrNoOfferHandler        = errors.New("no offer handler")
	ErrNoAnswerHandler       = errors.New("no answer handler")
)

//counterfeiter:generate . Handler
type Handler interface {
	OnICECandidate(c *webrtc.ICECandidate, target livekit.SignalTarget) error
	OnInitialConnected()
	OnFullyEstablished()
	OnFailed(isShortLived bool)
	OnTrack(track *webrtc.TrackRemote, rtpReceiver *webrtc.RTPReceiver)
	OnDataPacket(kind livekit.DataPacket_Kind, data []byte)
	OnOffer(sd webrtc.SessionDescription) error
	OnAnswer(sd webrtc.SessionDescription) error
	OnNegotiationStateChanged(state NegotiationState)
	OnNegotiationFailed()
	OnStreamStateChange(update *streamallocator.StreamStateUpdate) error
}

type UnimplementedHandler struct{}

func (h UnimplementedHandler) OnICECandidate(c *webrtc.ICECandidate, target livekit.SignalTarget) error {
	return ErrNoICECandidateHandler
}
func (h UnimplementedHandler) OnInitialConnected()                                                {}
func (h UnimplementedHandler) OnFullyEstablished()                                                {}
func (h UnimplementedHandler) OnFailed(isShortLived bool)                                         {}
func (h UnimplementedHandler) OnTrack(track *webrtc.TrackRemote, rtpReceiver *webrtc.RTPReceiver) {}
func (h UnimplementedHandler) OnDataPacket(kind livekit.DataPacket_Kind, data []byte)             {}
func (h UnimplementedHandler) OnOffer(sd webrtc.SessionDescription) error {
	return ErrNoOfferHandler
}
func (h UnimplementedHandler) OnAnswer(sd webrtc.SessionDescription) error {
	return ErrNoAnswerHandler
}
func (h UnimplementedHandler) OnNegotiationStateChanged(state NegotiationState) {}
func (h UnimplementedHandler) OnNegotiationFailed()                             {}
func (h UnimplementedHandler) OnStreamStateChange(update *streamallocator.StreamStateUpdate) error {
	return nil
}
