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

	"google.golang.org/protobuf/proto"
)

var _ ParticipantSignalling = (*signallingUnimplemented)(nil)

type signallingUnimplemented struct{}

func (u *signallingUnimplemented) SignalJoinResponse(join *livekit.JoinResponse) proto.Message {
	return nil
}

func (u *signallingUnimplemented) SignalParticipantUpdate(participants []*livekit.ParticipantInfo) proto.Message {
	return nil
}

func (u *signallingUnimplemented) SignalSpeakerUpdate(speakers []*livekit.SpeakerInfo) proto.Message {
	return nil
}

func (u *signallingUnimplemented) SignalRoomUpdate(room *livekit.Room) proto.Message {
	return nil
}

func (u *signallingUnimplemented) SignalConnectionQualityUpdate(connectionQuality *livekit.ConnectionQualityUpdate) proto.Message {
	return nil
}

func (u *signallingUnimplemented) SignalRefreshToken(token string) proto.Message {
	return nil
}

func (u *signallingUnimplemented) SignalRequestResponse(requestResponse *livekit.RequestResponse) proto.Message {
	return nil
}

func (u *signallingUnimplemented) SignalRoomMovedResponse(roomMoved *livekit.RoomMovedResponse) proto.Message {
	return nil
}

func (u *signallingUnimplemented) SignalReconnectResponse(reconnect *livekit.ReconnectResponse) proto.Message {
	return nil
}

func (u *signallingUnimplemented) SignalICECandidate(trickle *livekit.TrickleRequest) proto.Message {
	return nil
}

func (u *signallingUnimplemented) SignalTrackMuted(mute *livekit.MuteTrackRequest) proto.Message {
	return nil
}

func (u *signallingUnimplemented) SignalTrackPublished(trackPublished *livekit.TrackPublishedResponse) proto.Message {
	return nil
}

func (u *signallingUnimplemented) SignalTrackUnpublished(trackUnpublished *livekit.TrackUnpublishedResponse) proto.Message {
	return nil
}

func (u *signallingUnimplemented) SignalTrackSubscribed(trackSubscribed *livekit.TrackSubscribed) proto.Message {
	return nil
}

func (u *signallingUnimplemented) SignalLeaveRequest(leave *livekit.LeaveRequest) proto.Message {
	return nil
}

func (u *signallingUnimplemented) SignalSdpAnswer(answer *livekit.SessionDescription) proto.Message {
	return nil
}

func (u *signallingUnimplemented) SignalSdpOffer(offer *livekit.SessionDescription) proto.Message {
	return nil
}

func (u *signallingUnimplemented) SignalStreamStateUpdate(streamStateUpdate *livekit.StreamStateUpdate) proto.Message {
	return nil
}

func (u *signallingUnimplemented) SignalSubscribedQualityUpdate(subscribedQualityUpdate *livekit.SubscribedQualityUpdate) proto.Message {
	return nil
}

func (u *signallingUnimplemented) SignalSubscriptionResponse(subscriptionResponse *livekit.SubscriptionResponse) proto.Message {
	return nil
}

func (u *signallingUnimplemented) SignalSubscriptionPermissionUpdate(subscriptionPermissionUpdate *livekit.SubscriptionPermissionUpdate) proto.Message {
	return nil
}

func (u *signallingUnimplemented) SignalMediaSectionsRequirement(mediaSectionsRequirement *livekit.MediaSectionsRequirement) proto.Message {
	return nil
}

func (u *signallingUnimplemented) SignalSubscribedAudioCodecUpdate(subscribedAudioCodecUpdate *livekit.SubscribedAudioCodecUpdate) proto.Message {
	return nil
}
