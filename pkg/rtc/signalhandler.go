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

package rtc

import (
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/rtc/types"
)

func HandleParticipantSignal(room types.Room, participant types.LocalParticipant, req *livekit.SignalRequest, pLogger logger.Logger) error {
	participant.UpdateLastSeenSignal()

	switch msg := req.GetMessage().(type) {
	case *livekit.SignalRequest_Offer:
		participant.HandleOffer(FromProtoSessionDescription(msg.Offer))

	case *livekit.SignalRequest_Answer:
		participant.HandleAnswer(FromProtoSessionDescription(msg.Answer))

	case *livekit.SignalRequest_Trickle:
		candidateInit, err := FromProtoTrickle(msg.Trickle)
		if err != nil {
			pLogger.Warnw("could not decode trickle", err)
			return nil
		}
		participant.AddICECandidate(candidateInit, msg.Trickle.Target)

	case *livekit.SignalRequest_AddTrack:
		pLogger.Debugw("add track request", "trackID", msg.AddTrack.Cid)
		participant.AddTrack(msg.AddTrack)

	case *livekit.SignalRequest_Mute:
		participant.SetTrackMuted(livekit.TrackID(msg.Mute.Sid), msg.Mute.Muted, false)

	case *livekit.SignalRequest_Subscription:
		// allow participant to indicate their interest in the subscription
		// permission check happens later in SubscriptionManager
		room.UpdateSubscriptions(
			participant,
			livekit.StringsAsIDs[livekit.TrackID](msg.Subscription.TrackSids),
			msg.Subscription.ParticipantTracks,
			msg.Subscription.Subscribe,
		)

	case *livekit.SignalRequest_TrackSetting:
		for _, sid := range livekit.StringsAsIDs[livekit.TrackID](msg.TrackSetting.TrackSids) {
			participant.UpdateSubscribedTrackSettings(sid, msg.TrackSetting)
		}

	case *livekit.SignalRequest_Leave:
		pLogger.Debugw("client leaving room")
		room.RemoveParticipant(participant.Identity(), participant.ID(), types.ParticipantCloseReasonClientRequestLeave)

	case *livekit.SignalRequest_SubscriptionPermission:
		err := room.UpdateSubscriptionPermission(participant, msg.SubscriptionPermission)
		if err != nil {
			pLogger.Warnw("could not update subscription permission", err,
				"permissions", msg.SubscriptionPermission)
		}

	case *livekit.SignalRequest_SyncState:
		err := room.SyncState(participant, msg.SyncState)
		if err != nil {
			pLogger.Warnw("could not sync state", err,
				"state", msg.SyncState)
		}

	case *livekit.SignalRequest_Simulate:
		err := room.SimulateScenario(participant, msg.Simulate)
		if err != nil {
			pLogger.Warnw("could not simulate scenario", err,
				"simulate", msg.Simulate)
		}

	case *livekit.SignalRequest_PingReq:
		if msg.PingReq.Rtt > 0 {
			participant.UpdateSignalingRTT(uint32(msg.PingReq.Rtt))
		}

	case *livekit.SignalRequest_UpdateMetadata:
		var errorResponse *livekit.ErrorResponse
		if participant.ClaimGrants().Video.GetCanUpdateOwnMetadata() {
			err := room.UpdateParticipantMetadata(
				participant,
				msg.UpdateMetadata.Name,
				msg.UpdateMetadata.Metadata,
				msg.UpdateMetadata.Attributes,
			)
			if err != nil {
				pLogger.Warnw("could not update metadata", err)

				switch err {
				case ErrNameExceedsLimits:
					errorResponse = &livekit.ErrorResponse{
						Reason:  livekit.ErrorResponse_INVALID_ARGUMENT,
						Message: "exceeds name length limit",
					}
				case ErrMetadataExceedsLimits:
					errorResponse = &livekit.ErrorResponse{
						Reason:  livekit.ErrorResponse_INVALID_ARGUMENT,
						Message: "exceeds metadata size limit",
					}
				case ErrAttributeExceedsLimits:
					errorResponse = &livekit.ErrorResponse{
						Reason:  livekit.ErrorResponse_INVALID_ARGUMENT,
						Message: "exceeds attributes size limit",
					}
				}

			}
		} else {
			errorResponse = &livekit.ErrorResponse{
				Reason:  livekit.ErrorResponse_NOT_ALLOWED,
				Message: "does not have permission to update own metadata",
			}
		}
		if errorResponse != nil {
			errorResponse.RequestId = msg.UpdateMetadata.RequestId
			participant.SendErrorResponse(errorResponse)
		}

	case *livekit.SignalRequest_UpdateAudioTrack:
		if err := participant.UpdateAudioTrack(msg.UpdateAudioTrack); err != nil {
			pLogger.Warnw("could not update audio track", err, "update", msg.UpdateAudioTrack)
		}

	case *livekit.SignalRequest_UpdateVideoTrack:
		if err := participant.UpdateVideoTrack(msg.UpdateVideoTrack); err != nil {
			pLogger.Warnw("could not update video track", err, "update", msg.UpdateVideoTrack)
		}
	}

	return nil
}
