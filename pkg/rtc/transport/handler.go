// Copyright 2024 LiveKit, Inc.
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

package transport

import (
	"errors"

	"github.com/pion/webrtc/v4"

	"github.com/livekit/livekit-server/pkg/rtc/types"
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
	OnFailed(isShortLived bool, iceConnectionInfo *types.ICEConnectionInfo)
	OnTrack(track *webrtc.TrackRemote, rtpReceiver *webrtc.RTPReceiver)
	OnDataMessage(kind livekit.DataPacket_Kind, data []byte)
	OnDataMessageUnlabeled(data []byte)
	OnDataSendError(err error)
	OnOffer(sd webrtc.SessionDescription, offerId uint32) error
	OnSetRemoteDescriptionOffer()
	OnAnswer(sd webrtc.SessionDescription, answerId uint32, midToTrackID map[string]string) error
	OnNegotiationStateChanged(state NegotiationState)
	OnNegotiationFailed()
	OnStreamStateChange(update *streamallocator.StreamStateUpdate) error
	OnUnmatchedMedia(numAudios uint32, numVideos uint32) error
}

type UnimplementedHandler struct{}

func (h UnimplementedHandler) OnICECandidate(c *webrtc.ICECandidate, target livekit.SignalTarget) error {
	return ErrNoICECandidateHandler
}
func (h UnimplementedHandler) OnInitialConnected()                                                {}
func (h UnimplementedHandler) OnFullyEstablished()                                                {}
func (h UnimplementedHandler) OnFailed(isShortLived bool)                                         {}
func (h UnimplementedHandler) OnTrack(track *webrtc.TrackRemote, rtpReceiver *webrtc.RTPReceiver) {}
func (h UnimplementedHandler) OnDataMessage(kind livekit.DataPacket_Kind, data []byte)            {}
func (h UnimplementedHandler) OnDataMessageUnlabeled(data []byte)                                 {}
func (h UnimplementedHandler) OnDataSendError(err error)                                          {}
func (h UnimplementedHandler) OnOffer(sd webrtc.SessionDescription, offerId uint32) error {
	return ErrNoOfferHandler
}
func (h UnimplementedHandler) OnSetRemoteDescriptionOffer() {}
func (h UnimplementedHandler) OnAnswer(sd webrtc.SessionDescription, answerId uint32, midToTrackID map[string]string) error {
	return ErrNoAnswerHandler
}
func (h UnimplementedHandler) OnNegotiationStateChanged(state NegotiationState) {}
func (h UnimplementedHandler) OnNegotiationFailed()                             {}
func (h UnimplementedHandler) OnStreamStateChange(update *streamallocator.StreamStateUpdate) error {
	return nil
}
func (h UnimplementedHandler) OnUnmatchedMedia(numAudios uint32, numVideos uint32) error {
	return nil
}
