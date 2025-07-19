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
)

type ParticipantSignaller interface {
	SetResponseSink(sink routing.MessageSink)
	GetResponseSink() routing.MessageSink
	CloseSignalConnection(reason types.SignallingCloseReason)

	SendJoinResponse(join *livekit.JoinResponse) error
	SendParticipantUpdate(participants []*livekit.ParticipantInfo) error
	SendSpeakerUpdate(speakers []*livekit.SpeakerInfo) error
	SendRoomUpdate(room *livekit.Room) error
	SendConnectionQualityUpdate(connectionQuality *livekit.ConnectionQualityUpdate) error
	SendRefreshToken(token string) error
	SendRequestResponse(requestResponse *livekit.RequestResponse) error
	SendRoomMovedResponse(roomMoved *livekit.RoomMovedResponse) error
	SendReconnectResponse(reconnect *livekit.ReconnectResponse) error
	SendICECandidate(trickle *livekit.TrickleRequest) error
	SendTrackMuted(mute *livekit.MuteTrackRequest) error
	SendTrackPublished(trackPublished *livekit.TrackPublishedResponse) error
	SendTrackUnpublished(trackUnpublished *livekit.TrackUnpublishedResponse) error
	SendTrackSubscribed(trackSubscribed *livekit.TrackSubscribed) error
	SendLeaveRequest(leave *livekit.LeaveRequest) error
	SendSdpAnswer(answer *livekit.SessionDescription) error
	SendSdpOffer(offer *livekit.SessionDescription) error
	SendStreamStateUpdate(streamStateUpdate *livekit.StreamStateUpdate) error
	SendSubscribedQualityUpdate(subscribedQualityUpdate *livekit.SubscribedQualityUpdate) error
	SendSubscriptionResponse(subscriptionResponse *livekit.SubscriptionResponse) error
	SendSubscriptionPermissionUpdate(subscriptionPermissionUpdate *livekit.SubscriptionPermissionUpdate) error

	SetLastProcessedRemoteMessageId(lastProcessedRemoteMessageId uint32)

	SendConnectResponse(connectResponse *livekit.ConnectResponse) error
}
