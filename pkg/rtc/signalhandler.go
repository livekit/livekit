package rtc

import (
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/rtc/types"
)

func HandleParticipantSignal(room types.Room, participant types.LocalParticipant, req *livekit.SignalRequest, pLogger logger.Logger) error {
	participant.UpdateLastSeenSignal()

	switch msg := req.Message.(type) {
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
			livekit.StringsAsTrackIDs(msg.Subscription.TrackSids),
			msg.Subscription.ParticipantTracks,
			msg.Subscription.Subscribe,
		)
	case *livekit.SignalRequest_TrackSetting:
		for _, sid := range livekit.StringsAsTrackIDs(msg.TrackSetting.TrackSids) {
			participant.UpdateSubscribedTrackSettings(sid, msg.TrackSetting)
		}
	case *livekit.SignalRequest_Leave:
		pLogger.Infow("client leaving room")
		room.RemoveParticipant(participant.Identity(), participant.ID(), types.ParticipantCloseReasonClientRequestLeave)
	case *livekit.SignalRequest_UpdateLayers:
		err := room.UpdateVideoLayers(participant, msg.UpdateLayers)
		if err != nil {
			pLogger.Warnw("could not update video layers", err,
				"update", msg.UpdateLayers)
			return nil
		}
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
		if participant.ClaimGrants().Video.GetCanUpdateOwnMetadata() {
			if msg.UpdateMetadata.Metadata != "" {
				participant.SetMetadata(msg.UpdateMetadata.Metadata)
			}
			if msg.UpdateMetadata.Name != "" {
				participant.SetName(msg.UpdateMetadata.Name)
			}
		}
	}
	return nil
}
