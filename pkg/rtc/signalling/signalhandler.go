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
	protosignalling "github.com/livekit/protocol/signalling"
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

	switch msg := req.GetMessage().(type) {
	case *livekit.SignalRequest_Offer:
		s.params.Participant.HandleOffer(protosignalling.FromProtoSessionDescription(msg.Offer))

	case *livekit.SignalRequest_Answer:
		s.params.Participant.HandleAnswer(protosignalling.FromProtoSessionDescription(msg.Answer))

	case *livekit.SignalRequest_Trickle:
		candidateInit, err := protosignalling.FromProtoTrickle(msg.Trickle)
		if err != nil {
			s.params.Logger.Warnw("could not decode trickle", err)
			return err
		}
		s.params.Participant.AddICECandidate(candidateInit, msg.Trickle.Target)

	case *livekit.SignalRequest_AddTrack:
		s.params.Logger.Debugw("add track request", "trackID", msg.AddTrack.Cid)
		s.params.Participant.AddTrack(msg.AddTrack)

	case *livekit.SignalRequest_Mute:
		s.params.Participant.SetTrackMuted(livekit.TrackID(msg.Mute.Sid), msg.Mute.Muted, false)

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
		requestResponse := &livekit.RequestResponse{
			RequestId: msg.UpdateMetadata.RequestId,
			Reason:    livekit.RequestResponse_OK,
		}
		if s.params.Participant.ClaimGrants().Video.GetCanUpdateOwnMetadata() {
			if err := s.params.Participant.CheckMetadataLimits(
				msg.UpdateMetadata.Name,
				msg.UpdateMetadata.Metadata,
				msg.UpdateMetadata.Attributes,
			); err == nil {
				if msg.UpdateMetadata.Name != "" {
					s.params.Participant.SetName(msg.UpdateMetadata.Name)
				}
				if msg.UpdateMetadata.Metadata != "" {
					s.params.Participant.SetMetadata(msg.UpdateMetadata.Metadata)
				}
				if msg.UpdateMetadata.Attributes != nil {
					s.params.Participant.SetAttributes(msg.UpdateMetadata.Attributes)
				}
			} else {
				s.params.Logger.Warnw("could not update metadata", err)

				switch err {
				case ErrNameExceedsLimits:
					requestResponse.Reason = livekit.RequestResponse_LIMIT_EXCEEDED
					requestResponse.Message = "exceeds name length limit"

				case ErrMetadataExceedsLimits:
					requestResponse.Reason = livekit.RequestResponse_LIMIT_EXCEEDED
					requestResponse.Message = "exceeds metadata size limit"

				case ErrAttributesExceedsLimits:
					requestResponse.Reason = livekit.RequestResponse_LIMIT_EXCEEDED
					requestResponse.Message = "exceeds attributes size limit"
				}

			}
		} else {
			requestResponse.Reason = livekit.RequestResponse_NOT_ALLOWED
			requestResponse.Message = "does not have permission to update own metadata"
		}
		s.params.Participant.SendRequestResponse(requestResponse)

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
