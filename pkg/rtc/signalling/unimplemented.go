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

type unimplemented struct{}

func (u *unimplemented) SetResponseSink(sink routing.MessageSink) {}

func (u *unimplemented) GetResponseSink() routing.MessageSink {
	return nil
}

func (u *unimplemented) CloseSignalConnection(reason types.SignallingCloseReason) {}

func (u *unimplemented) SendJoinResponse(join *livekit.JoinResponse) error {
	return nil
}

func (u *unimplemented) SendParticipantUpdate(participants []*livekit.ParticipantInfo) error {
	return nil
}

func (u *unimplemented) SendSpeakerUpdate(speakers []*livekit.SpeakerInfo) error {
	return nil
}

func (u *unimplemented) SendRoomUpdate(room *livekit.Room) error {
	return nil
}

func (u *unimplemented) SendConnectionQualityUpdate(connectionQuality *livekit.ConnectionQualityUpdate) error {
	return nil
}

func (u *unimplemented) SendRefreshToken(token string) error {
	return nil
}

func (u *unimplemented) SendRequestResponse(requestResponse *livekit.RequestResponse) error {
	return nil
}

func (u *unimplemented) SendRoomMovedResponse(roomMoved *livekit.RoomMovedResponse) error {
	return nil
}

func (u *unimplemented) SendReconnectResponse(reconnect *livekit.ReconnectResponse) error {
	return nil
}

func (u *unimplemented) SendICECandidate(trickle *livekit.TrickleRequest) error {
	return nil
}

func (u *unimplemented) SendTrackMuted(mute *livekit.MuteTrackRequest) error {
	return nil
}

func (u *unimplemented) SendTrackPublished(trackPublished *livekit.TrackPublishedResponse) error {
	return nil
}

func (u *unimplemented) SendTrackUnpublished(trackUnpublished *livekit.TrackUnpublishedResponse) error {
	return nil
}

func (u *unimplemented) SendTrackSubscribed(trackSubscribed *livekit.TrackSubscribed) error {
	return nil
}

func (u *unimplemented) SendLeaveRequest(leave *livekit.LeaveRequest) error {
	return nil
}

func (u *unimplemented) SendSdpAnswer(answer *livekit.SessionDescription) error {
	return nil
}

func (u *unimplemented) SendSdpOffer(offer *livekit.SessionDescription) error {
	return nil
}

func (u *unimplemented) SendStreamStateUpdate(streamStateUpdate *livekit.StreamStateUpdate) error {
	return nil
}

func (u *unimplemented) SendSubscribedQualityUpdate(subscribedQualityUpdate *livekit.SubscribedQualityUpdate) error {
	return nil
}

func (u *unimplemented) SendSubscriptionResponse(subscriptionResponse *livekit.SubscriptionResponse) error {
	return nil
}

func (u *unimplemented) SendSubscriptionPermissionUpdate(subscriptionPermissionUpdate *livekit.SubscriptionPermissionUpdate) error {
	return nil
}

func (u *unimplemented) SetLastProcessedRemoteMessageId(lastProcessedRemoteMessageId uint32) {}

func (u *unimplemented) SendConnectResponse(connectResponse *livekit.ConnectResponse) error {
	return nil
}
