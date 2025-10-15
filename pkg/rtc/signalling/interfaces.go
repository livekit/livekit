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

	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/rtc/types"

	"google.golang.org/protobuf/proto"
)

type ParticipantSignalHandler interface {
	HandleMessage(msg proto.Message) error
}

type ParticipantSignaller interface {
	SetResponseSink(sink routing.MessageSink)
	GetResponseSink() routing.MessageSink
	CloseSignalConnection(reason types.SignallingCloseReason)

	WriteMessage(msg proto.Message) error
}

type ParticipantSignalling interface {
	SignalJoinResponse(join *livekit.JoinResponse) proto.Message
	SignalParticipantUpdate(participants []*livekit.ParticipantInfo) proto.Message
	SignalSpeakerUpdate(speakers []*livekit.SpeakerInfo) proto.Message
	SignalRoomUpdate(room *livekit.Room) proto.Message
	SignalConnectionQualityUpdate(connectionQuality *livekit.ConnectionQualityUpdate) proto.Message
	SignalRefreshToken(token string) proto.Message
	SignalRequestResponse(requestResponse *livekit.RequestResponse) proto.Message
	SignalRoomMovedResponse(roomMoved *livekit.RoomMovedResponse) proto.Message
	SignalReconnectResponse(reconnect *livekit.ReconnectResponse) proto.Message
	SignalICECandidate(trickle *livekit.TrickleRequest) proto.Message
	SignalTrackMuted(mute *livekit.MuteTrackRequest) proto.Message
	SignalTrackPublished(trackPublished *livekit.TrackPublishedResponse) proto.Message
	SignalTrackUnpublished(trackUnpublished *livekit.TrackUnpublishedResponse) proto.Message
	SignalTrackSubscribed(trackSubscribed *livekit.TrackSubscribed) proto.Message
	SignalLeaveRequest(leave *livekit.LeaveRequest) proto.Message
	SignalSdpAnswer(answer *livekit.SessionDescription) proto.Message
	SignalMappedSdpAnswer(mappedAnswer *livekit.MappedSessionDescription) proto.Message
	SignalSdpOffer(offer *livekit.SessionDescription) proto.Message
	SignalStreamStateUpdate(streamStateUpdate *livekit.StreamStateUpdate) proto.Message
	SignalSubscribedQualityUpdate(subscribedQualityUpdate *livekit.SubscribedQualityUpdate) proto.Message
	SignalSubscriptionResponse(subscriptionResponse *livekit.SubscriptionResponse) proto.Message
	SignalSubscriptionPermissionUpdate(subscriptionPermissionUpdate *livekit.SubscriptionPermissionUpdate) proto.Message
	SignalMediaSectionsRequirement(mediaSectionsRequirement *livekit.MediaSectionsRequirement) proto.Message
	SignalSubscribedAudioCodecUpdate(subscribedAudioCodecUpdate *livekit.SubscribedAudioCodecUpdate) proto.Message
}
