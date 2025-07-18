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

package signalling

import (
	"fmt"
	"sync"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils"
	"github.com/livekit/psrpc"

	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/rtc/types"
)

type SignallerParams struct {
	Logger      logger.Logger
	Participant types.LocalParticipant
}

type signaller struct {
	unimplemented

	params SignallerParams

	resSinkMu sync.Mutex
	resSink   routing.MessageSink
}

func NewSignaller(params SignallerParams) ParticipantSignaller {
	return &signaller{
		params: params,
	}
}

func (s *signaller) SetResponseSink(sink routing.MessageSink) {
	s.resSinkMu.Lock()
	defer s.resSinkMu.Unlock()
	s.resSink = sink
}

func (s *signaller) GetResponseSink() routing.MessageSink {
	s.resSinkMu.Lock()
	defer s.resSinkMu.Unlock()
	return s.resSink
}

// closes signal connection to notify client to resume/reconnect
func (s *signaller) CloseSignalConnection(reason types.SignallingCloseReason) {
	sink := s.GetResponseSink()
	if sink == nil {
		return
	}

	s.params.Logger.Debugw("closing signal connection", "reason", reason, "connID", sink.ConnectionID())
	sink.Close()
	s.SetResponseSink(nil)
}

func (s *signaller) SendJoinResponse(join *livekit.JoinResponse) error {
	if join == nil {
		return nil
	}

	return s.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_Join{
			Join: join,
		},
	})
}

func (s *signaller) SendParticipantUpdate(participants []*livekit.ParticipantInfo) error {
	if len(participants) == 0 {
		return nil
	}

	return s.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_Update{
			Update: &livekit.ParticipantUpdate{
				Participants: participants,
			},
		},
	})
}

func (s *signaller) SendSpeakerUpdate(speakers []*livekit.SpeakerInfo) error {
	if len(speakers) == 0 {
		return nil
	}

	return s.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_SpeakersChanged{
			SpeakersChanged: &livekit.SpeakersChanged{
				Speakers: speakers,
			},
		},
	})
}

func (s *signaller) SendRoomUpdate(room *livekit.Room) error {
	if room == nil {
		return nil
	}

	return s.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_RoomUpdate{
			RoomUpdate: &livekit.RoomUpdate{
				Room: room,
			},
		},
	})
}

func (s *signaller) SendConnectionQualityUpdate(connectionQuality *livekit.ConnectionQualityUpdate) error {
	if connectionQuality == nil {
		return nil
	}

	return s.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_ConnectionQuality{
			ConnectionQuality: connectionQuality,
		},
	})
}

func (s *signaller) SendRefreshToken(token string) error {
	if token == "" {
		return nil
	}

	return s.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_RefreshToken{
			RefreshToken: token,
		},
	})
}

func (s *signaller) SendRequestResponse(requestResponse *livekit.RequestResponse) error {
	if requestResponse == nil {
		return nil
	}

	return s.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_RequestResponse{
			RequestResponse: requestResponse,
		},
	})
}

func (s *signaller) SendRoomMovedResponse(roomMoved *livekit.RoomMovedResponse) error {
	if roomMoved == nil {
		return nil
	}

	return s.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_RoomMoved{
			RoomMoved: roomMoved,
		},
	})
}

func (s *signaller) SendReconnectResponse(reconnect *livekit.ReconnectResponse) error {
	if reconnect == nil {
		return nil
	}

	return s.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_Reconnect{
			Reconnect: reconnect,
		},
	})
}

func (s *signaller) SendICECandidate(trickle *livekit.TrickleRequest) error {
	if trickle == nil {
		return nil
	}

	return s.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_Trickle{
			Trickle: trickle,
		},
	})
}

func (s *signaller) SendTrackMuted(mute *livekit.MuteTrackRequest) error {
	if mute == nil {
		return nil
	}

	return s.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_Mute{
			Mute: mute,
		},
	})
}

func (s *signaller) SendTrackPublished(trackPublished *livekit.TrackPublishedResponse) error {
	if trackPublished == nil {
		return nil
	}

	return s.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_TrackPublished{
			TrackPublished: trackPublished,
		},
	})
}

func (s *signaller) SendTrackUnpublished(trackUnpublished *livekit.TrackUnpublishedResponse) error {
	if trackUnpublished == nil {
		return nil
	}

	return s.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_TrackUnpublished{
			TrackUnpublished: trackUnpublished,
		},
	})
}

func (s *signaller) SendTrackSubscribed(trackSubscribed *livekit.TrackSubscribed) error {
	if trackSubscribed == nil {
		return nil
	}

	return s.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_TrackSubscribed{
			TrackSubscribed: trackSubscribed,
		},
	})
}

func (s *signaller) SendLeaveRequest(leave *livekit.LeaveRequest) error {
	if leave == nil {
		return nil
	}

	return s.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_Leave{
			Leave: leave,
		},
	})
}

func (s *signaller) SendSdpAnswer(answer *livekit.SessionDescription) error {
	if answer == nil {
		return nil
	}

	return s.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_Answer{
			Answer: answer,
		},
	})
}

func (s *signaller) SendSdpOffer(offer *livekit.SessionDescription) error {
	if offer == nil {
		return nil
	}

	return s.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_Offer{
			Offer: offer,
		},
	})
}

func (s *signaller) SendStreamStateUpdate(streamStateUpdate *livekit.StreamStateUpdate) error {
	if streamStateUpdate == nil {
		return nil
	}

	return s.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_StreamStateUpdate{
			StreamStateUpdate: streamStateUpdate,
		},
	})
}

func (s *signaller) SendSubscribedQualityUpdate(subscribedQualityUpdate *livekit.SubscribedQualityUpdate) error {
	if subscribedQualityUpdate == nil {
		return nil
	}

	return s.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_SubscribedQualityUpdate{
			SubscribedQualityUpdate: subscribedQualityUpdate,
		},
	})
}

func (s *signaller) SendSubscriptionResponse(subscriptionResponse *livekit.SubscriptionResponse) error {
	if subscriptionResponse == nil {
		return nil
	}

	return s.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_SubscriptionResponse{
			SubscriptionResponse: subscriptionResponse,
		},
	})
}

func (s *signaller) SendSubscriptionPermissionUpdate(subscriptionPermissionUpdate *livekit.SubscriptionPermissionUpdate) error {
	if subscriptionPermissionUpdate == nil {
		return nil
	}

	return s.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_SubscriptionPermissionUpdate{
			SubscriptionPermissionUpdate: subscriptionPermissionUpdate,
		},
	})
}

func (s *signaller) writeMessage(msg *livekit.SignalResponse) error {
	if s.params.Participant.IsDisconnected() || (!s.params.Participant.IsReady() && msg.GetJoin() == nil) {
		return nil
	}

	sink := s.GetResponseSink()
	if sink == nil {
		s.params.Logger.Debugw(
			"could not send message to participant",
			"messageType", fmt.Sprintf("%T", msg.Message),
		)
		return nil
	}

	err := sink.WriteMessage(msg)
	if utils.ErrorIsOneOf(err, psrpc.Canceled, routing.ErrChannelClosed) {
		s.params.Logger.Debugw(
			"could not send message to participant",
			"error", err,
			"messageType", fmt.Sprintf("%T", msg.Message),
		)
		return nil
	} else if err != nil {
		s.params.Logger.Warnw(
			"could not send message to participant", err,
			"messageType", fmt.Sprintf("%T", msg.Message),
		)
		return err
	}
	return nil
}
