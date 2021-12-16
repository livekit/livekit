package rtc

import (
	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
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
		participant.SetTrackMuted(msg.Mute.Sid, msg.Mute.Muted, false)
	case *livekit.SignalRequest_Subscription:
		var err error
		if participant.CanSubscribe() {
			updateErr := room.UpdateSubscriptions(participant, msg.Subscription.TrackSids, msg.Subscription.Subscribe)
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
		for _, sid := range msg.TrackSetting.TrackSids {
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
		track := participant.GetPublishedTrack(msg.UpdateLayers.TrackSid)
		if track == nil {
			pLogger.Warnw("could not find published track", nil,
				"track", msg.UpdateLayers.TrackSid)
			return nil
		}
		track.UpdateVideoLayers(msg.UpdateLayers.Layers)
	case *livekit.SignalRequest_Leave:
		_ = participant.Close()
	}
	return nil
}
