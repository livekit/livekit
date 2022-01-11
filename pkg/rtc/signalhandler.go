package rtc

import (
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/rtc/types"
)

func HandleParticipantSignal(room types.Room, participant types.Participant, req *livekit.SignalRequest, pLogger logger.Logger) error {
	switch msg := req.Message.(type) {
	case *livekit.SignalRequest_Offer:
		_, err := participant.HandleOffer(FromProtoSessionDescription(msg.Offer))
		if err != nil {
			pLogger.Errorw("could not handle offer", err)
			return err
		}
	case *livekit.SignalRequest_AddTrack:
		pLogger.Debugw("add track request", "track", msg.AddTrack.Cid)
		participant.AddTrack(msg.AddTrack)
	case *livekit.SignalRequest_Answer:
		sd := FromProtoSessionDescription(msg.Answer)
		if err := participant.HandleAnswer(sd); err != nil {
			pLogger.Errorw("could not handle answer", err)
			// connection cannot be successful if we can't answer
			return err
		}
	case *livekit.SignalRequest_Trickle:
		candidateInit, err := FromProtoTrickle(msg.Trickle)
		if err != nil {
			pLogger.Warnw("could not decode trickle", err)
			return nil
		}
		if err := participant.AddICECandidate(candidateInit, msg.Trickle.Target); err != nil {
			pLogger.Warnw("could not handle trickle", err)
		}
	case *livekit.SignalRequest_Mute:
		participant.SetTrackMuted(livekit.TrackID(msg.Mute.Sid), msg.Mute.Muted, false)
	case *livekit.SignalRequest_Subscription:
		var err error
		if participant.CanSubscribe() {
			updateErr := room.UpdateSubscriptions(
				participant,
				livekit.StringsAsTrackIDs(msg.Subscription.TrackSids),
				msg.Subscription.ParticipantTracks,
				msg.Subscription.Subscribe,
			)
			if updateErr != nil {
				err = updateErr
			}
		} else {
			err = ErrCannotSubscribe
		}
		if err != nil {
			pLogger.Warnw("could not update subscription", err,
				"tracks", msg.Subscription.TrackSids,
				"subscribe", msg.Subscription.Subscribe)
		}
	case *livekit.SignalRequest_TrackSetting:
		for _, sid := range livekit.StringsAsTrackIDs(msg.TrackSetting.TrackSids) {
			subTrack := participant.GetSubscribedTrack(sid)
			if subTrack == nil {
				pLogger.Warnw("unable to find SubscribedTrack", nil,
					"track", sid)
				continue
			}

			// find quality for published track
			pLogger.Debugw("updating track settings",
				"settings", msg.TrackSetting)
			subTrack.UpdateSubscriberSettings(msg.TrackSetting)
		}
	case *livekit.SignalRequest_UpdateLayers:
		err := room.UpdateVideoLayers(participant, msg.UpdateLayers)
		if err != nil {
			pLogger.Warnw("could not update video layers", err,
				"update", msg.UpdateLayers)
			return nil
		}
	case *livekit.SignalRequest_Leave:
		_ = participant.Close(true)
	case *livekit.SignalRequest_SubscriptionPermissions:
		err := room.UpdateSubscriptionPermissions(participant, msg.SubscriptionPermissions)
		if err != nil {
			pLogger.Warnw("could not update subscription permissions", err,
				"permissions", msg.SubscriptionPermissions)
		}
	case *livekit.SignalRequest_SyncState:
		err := room.SyncState(participant, msg.SyncState)
		if err != nil {
			pLogger.Warnw("could not sync subscribe state", err,
				"state", msg.SyncState)
		}
	case *livekit.SignalRequest_Simulate:
		err := room.SimulateScenario(participant, msg.Simulate)
		if err != nil {
			pLogger.Warnw("could not simulate scenario", err,
				"simulate", msg.Simulate)
		}
	}
	return nil
}
