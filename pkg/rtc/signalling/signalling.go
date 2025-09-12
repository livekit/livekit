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
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"google.golang.org/protobuf/proto"
)

var _ ParticipantSignalling = (*signalling)(nil)

type SignallingParams struct {
	Logger logger.Logger
}

type signalling struct {
	signallingUnimplemented

	params SignallingParams
}

func NewSignalling(params SignallingParams) ParticipantSignalling {
	return &signalling{
		params: params,
	}
}

func (s *signalling) SignalJoinResponse(join *livekit.JoinResponse) proto.Message {
	return &livekit.SignalResponse{
		Message: &livekit.SignalResponse_Join{
			Join: join,
		},
	}
}

func (s *signalling) SignalParticipantUpdate(participants []*livekit.ParticipantInfo) proto.Message {
	if len(participants) == 0 {
		return nil
	}

	return &livekit.SignalResponse{
		Message: &livekit.SignalResponse_Update{
			Update: &livekit.ParticipantUpdate{
				Participants: participants,
			},
		},
	}
}

func (s *signalling) SignalSpeakerUpdate(speakers []*livekit.SpeakerInfo) proto.Message {
	if len(speakers) == 0 {
		return nil
	}

	return &livekit.SignalResponse{
		Message: &livekit.SignalResponse_SpeakersChanged{
			SpeakersChanged: &livekit.SpeakersChanged{
				Speakers: speakers,
			},
		},
	}
}

func (s *signalling) SignalRoomUpdate(room *livekit.Room) proto.Message {
	return &livekit.SignalResponse{
		Message: &livekit.SignalResponse_RoomUpdate{
			RoomUpdate: &livekit.RoomUpdate{
				Room: room,
			},
		},
	}
}

func (s *signalling) SignalConnectionQualityUpdate(connectionQuality *livekit.ConnectionQualityUpdate) proto.Message {
	return &livekit.SignalResponse{
		Message: &livekit.SignalResponse_ConnectionQuality{
			ConnectionQuality: connectionQuality,
		},
	}
}

func (s *signalling) SignalRefreshToken(token string) proto.Message {
	return &livekit.SignalResponse{
		Message: &livekit.SignalResponse_RefreshToken{
			RefreshToken: token,
		},
	}
}

func (s *signalling) SignalRequestResponse(requestResponse *livekit.RequestResponse) proto.Message {
	return &livekit.SignalResponse{
		Message: &livekit.SignalResponse_RequestResponse{
			RequestResponse: requestResponse,
		},
	}
}

func (s *signalling) SignalRoomMovedResponse(roomMoved *livekit.RoomMovedResponse) proto.Message {
	return &livekit.SignalResponse{
		Message: &livekit.SignalResponse_RoomMoved{
			RoomMoved: roomMoved,
		},
	}
}

func (s *signalling) SignalReconnectResponse(reconnect *livekit.ReconnectResponse) proto.Message {
	return &livekit.SignalResponse{
		Message: &livekit.SignalResponse_Reconnect{
			Reconnect: reconnect,
		},
	}
}

func (s *signalling) SignalICECandidate(trickle *livekit.TrickleRequest) proto.Message {
	return &livekit.SignalResponse{
		Message: &livekit.SignalResponse_Trickle{
			Trickle: trickle,
		},
	}
}

func (s *signalling) SignalTrackMuted(mute *livekit.MuteTrackRequest) proto.Message {
	return &livekit.SignalResponse{
		Message: &livekit.SignalResponse_Mute{
			Mute: mute,
		},
	}
}

func (s *signalling) SignalTrackPublished(trackPublished *livekit.TrackPublishedResponse) proto.Message {
	return &livekit.SignalResponse{
		Message: &livekit.SignalResponse_TrackPublished{
			TrackPublished: trackPublished,
		},
	}
}

func (s *signalling) SignalTrackUnpublished(trackUnpublished *livekit.TrackUnpublishedResponse) proto.Message {
	return &livekit.SignalResponse{
		Message: &livekit.SignalResponse_TrackUnpublished{
			TrackUnpublished: trackUnpublished,
		},
	}
}

func (s *signalling) SignalTrackSubscribed(trackSubscribed *livekit.TrackSubscribed) proto.Message {
	return &livekit.SignalResponse{
		Message: &livekit.SignalResponse_TrackSubscribed{
			TrackSubscribed: trackSubscribed,
		},
	}
}

func (s *signalling) SignalLeaveRequest(leave *livekit.LeaveRequest) proto.Message {
	return &livekit.SignalResponse{
		Message: &livekit.SignalResponse_Leave{
			Leave: leave,
		},
	}
}

func (s *signalling) SignalSdpAnswer(answer *livekit.SessionDescription) proto.Message {
	return &livekit.SignalResponse{
		Message: &livekit.SignalResponse_Answer{
			Answer: answer,
		},
	}
}

func (s *signalling) SignalSdpOffer(offer *livekit.SessionDescription) proto.Message {
	return &livekit.SignalResponse{
		Message: &livekit.SignalResponse_Offer{
			Offer: offer,
		},
	}
}

func (s *signalling) SignalStreamStateUpdate(streamStateUpdate *livekit.StreamStateUpdate) proto.Message {
	return &livekit.SignalResponse{
		Message: &livekit.SignalResponse_StreamStateUpdate{
			StreamStateUpdate: streamStateUpdate,
		},
	}
}

func (s *signalling) SignalSubscribedQualityUpdate(subscribedQualityUpdate *livekit.SubscribedQualityUpdate) proto.Message {
	return &livekit.SignalResponse{
		Message: &livekit.SignalResponse_SubscribedQualityUpdate{
			SubscribedQualityUpdate: subscribedQualityUpdate,
		},
	}
}

func (s *signalling) SignalSubscriptionResponse(subscriptionResponse *livekit.SubscriptionResponse) proto.Message {
	return &livekit.SignalResponse{
		Message: &livekit.SignalResponse_SubscriptionResponse{
			SubscriptionResponse: subscriptionResponse,
		},
	}
}

func (s *signalling) SignalSubscriptionPermissionUpdate(subscriptionPermissionUpdate *livekit.SubscriptionPermissionUpdate) proto.Message {
	return &livekit.SignalResponse{
		Message: &livekit.SignalResponse_SubscriptionPermissionUpdate{
			SubscriptionPermissionUpdate: subscriptionPermissionUpdate,
		},
	}
}

func (u *signalling) SignalMediaSectionsRequirement(mediaSectionsRequirement *livekit.MediaSectionsRequirement) proto.Message {
	return &livekit.SignalResponse{
		Message: &livekit.SignalResponse_MediaSectionsRequirement{
			MediaSectionsRequirement: mediaSectionsRequirement,
		},
	}
}

func (s *signalling) SignalSubscribedAudioCodecUpdate(subscribedAudioCodecUpdate *livekit.SubscribedAudioCodecUpdate) proto.Message {
	return &livekit.SignalResponse{
		Message: &livekit.SignalResponse_SubscribedAudioCodecUpdate{
			SubscribedAudioCodecUpdate: subscribedAudioCodecUpdate,
		},
	}
}
