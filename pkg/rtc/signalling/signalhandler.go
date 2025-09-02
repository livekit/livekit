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

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/livekit-server/pkg/rtc/types"
)

var _ ParticipantSignalHandler = (*signalhandler)(nil)

type SignalHandlerParams struct {
	Logger      logger.Logger
	Participant types.LocalParticipant
}

type signalhandler struct {
	signalhandlerUnimplemented

	params SignalHandlerParams
}

func NewSignalHandler(params SignalHandlerParams) ParticipantSignalHandler {
	return &signalhandler{
		params: params,
	}
}

func (s *signalhandler) HandleMessage(msg proto.Message) error {
	req, ok := msg.(*livekit.SignalRequest)
	if !ok {
		s.params.Logger.Warnw(
			"unknown message type", nil,
			"messageType", fmt.Sprintf("%T", msg),
		)
		return ErrInvalidMessageType
	}
	s.params.Participant.UpdateLastSeenSignal()

	s.params.Logger.Debugw("handling signal request", "request", logger.Proto(req))
	switch msg := req.GetMessage().(type) {
	case *livekit.SignalRequest_Offer:
		s.params.Participant.HandleOffer(msg.Offer)

	case *livekit.SignalRequest_Answer:
		s.params.Participant.HandleAnswer(msg.Answer)

	case *livekit.SignalRequest_Trickle:
		s.params.Participant.HandleICETrickle(msg.Trickle)

	case *livekit.SignalRequest_AddTrack:
		s.params.Participant.AddTrack(msg.AddTrack)

	case *livekit.SignalRequest_Mute:
		s.params.Participant.SetTrackMuted(msg.Mute, false)

	case *livekit.SignalRequest_Subscription:
		// allow participant to indicate their interest in the subscription
		// permission check happens later in SubscriptionManager
		s.params.Participant.HandleUpdateSubscriptions(
			livekit.StringsAsIDs[livekit.TrackID](msg.Subscription.TrackSids),
			msg.Subscription.ParticipantTracks,
			msg.Subscription.Subscribe,
		)

	case *livekit.SignalRequest_TrackSetting:
		for _, sid := range livekit.StringsAsIDs[livekit.TrackID](msg.TrackSetting.TrackSids) {
			s.params.Participant.UpdateSubscribedTrackSettings(sid, msg.TrackSetting)
		}

	case *livekit.SignalRequest_Leave:
		reason := types.ParticipantCloseReasonClientRequestLeave
		switch msg.Leave.Reason {
		case livekit.DisconnectReason_CLIENT_INITIATED:
			reason = types.ParticipantCloseReasonClientRequestLeave
		case livekit.DisconnectReason_USER_UNAVAILABLE:
			reason = types.ParticipantCloseReasonUserUnavailable
		case livekit.DisconnectReason_USER_REJECTED:
			reason = types.ParticipantCloseReasonUserRejected
		}
		s.params.Logger.Debugw("client leaving room", "reason", reason)
		s.params.Participant.HandleLeaveRequest(reason)

	case *livekit.SignalRequest_SubscriptionPermission:
		err := s.params.Participant.HandleUpdateSubscriptionPermission(msg.SubscriptionPermission)
		if err != nil {
			s.params.Logger.Warnw(
				"could not update subscription permission", err,
				"permissions", msg.SubscriptionPermission,
			)
		}

	case *livekit.SignalRequest_SyncState:
		err := s.params.Participant.HandleSyncState(msg.SyncState)
		if err != nil {
			s.params.Logger.Warnw(
				"could not sync state", err,
				"state", msg.SyncState,
			)
		}

	case *livekit.SignalRequest_Simulate:
		err := s.params.Participant.HandleSimulateScenario(msg.Simulate)
		if err != nil {
			s.params.Logger.Warnw(
				"could not simulate scenario", err,
				"simulate", msg.Simulate,
			)
		}

	case *livekit.SignalRequest_PingReq:
		if msg.PingReq.Rtt > 0 {
			s.params.Participant.UpdateSignalingRTT(uint32(msg.PingReq.Rtt))
		}

	case *livekit.SignalRequest_UpdateMetadata:
		s.params.Participant.UpdateMetadata(msg.UpdateMetadata, false)

	case *livekit.SignalRequest_UpdateAudioTrack:
		if err := s.params.Participant.UpdateAudioTrack(msg.UpdateAudioTrack); err != nil {
			s.params.Logger.Warnw("could not update audio track", err, "update", msg.UpdateAudioTrack)
		}

	case *livekit.SignalRequest_UpdateVideoTrack:
		if err := s.params.Participant.UpdateVideoTrack(msg.UpdateVideoTrack); err != nil {
			s.params.Logger.Warnw("could not update video track", err, "update", msg.UpdateVideoTrack)
		}
	}

	return nil
}
