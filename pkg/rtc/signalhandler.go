package rtc

import (
	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
)

func HandleParticipantSignal(room types.Room, participant types.Participant, req *livekit.SignalRequest) error {
	switch msg := req.Message.(type) {
	case *livekit.SignalRequest_Offer:
		_, err := participant.HandleOffer(FromProtoSessionDescription(msg.Offer))
		if err != nil {
			logger.Errorw("could not handle offer", err,
				"room", room.Name(),
				"participant", participant.Identity(),
				"pID", participant.ID(),
			)
			return err
		}
	case *livekit.SignalRequest_AddTrack:
		logger.Debugw("add track request",
			"room", room.Name(),
			"participant", participant.Identity(),
			"pID", participant.ID(),
			"track", msg.AddTrack.Cid)
		participant.AddTrack(msg.AddTrack)
	case *livekit.SignalRequest_Answer:
		sd := FromProtoSessionDescription(msg.Answer)
		if err := participant.HandleAnswer(sd); err != nil {
			logger.Errorw("could not handle answer", err,
				"room", room.Name(),
				"participant", participant.Identity(),
				"pID", participant.ID(),
			)
			// connection cannot be successful if we can't answer
			return err
		}
	case *livekit.SignalRequest_Trickle:
		candidateInit, err := FromProtoTrickle(msg.Trickle)
		if err != nil {
			logger.Warnw("could not decode trickle", err,
				"room", room.Name(),
				"participant", participant.Identity(),
				"pID", participant.ID(),
			)
			return nil
		}
		// logger.Debugw("adding peer candidate", "participant", participant.Identity())
		if err := participant.AddICECandidate(candidateInit, msg.Trickle.Target); err != nil {
			logger.Warnw("could not handle trickle", err,
				"room", room.Name(),
				"participant", participant.Identity(),
				"pID", participant.ID(),
			)
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
			logger.Warnw("could not update subscription", err,
				"room", room.Name(),
				"participant", participant.Identity(),
				"pID", participant.ID(),
				"tracks", msg.Subscription.TrackSids,
				"subscribe", msg.Subscription.Subscribe)
		}
	case *livekit.SignalRequest_TrackSetting:
		for _, sid := range msg.TrackSetting.TrackSids {
			subTrack := participant.GetSubscribedTrack(sid)
			if subTrack == nil {
				logger.Warnw("unable to find SubscribedTrack", nil,
					"room", room.Name(),
					"participant", participant.Identity(),
					"pID", participant.ID(),
					"track", sid)
				continue
			}

			// find quality for published track
			logger.Debugw("updating track settings",
				"room", room.Name(),
				"participant", participant.Identity(),
				"pID", participant.ID(),
				"settings", msg.TrackSetting)
			subTrack.UpdateSubscriberSettings(msg.TrackSetting)
		}
	case *livekit.SignalRequest_UpdateLayers:
		track := participant.GetPublishedTrack(msg.UpdateLayers.TrackSid)
		if track == nil {
			logger.Warnw("could not find published track", nil,
				"track", msg.UpdateLayers.TrackSid)
			return nil
		}
		track.UpdateVideoLayers(msg.UpdateLayers.Layers)
	case *livekit.SignalRequest_Leave:
		_ = participant.Close()
	}
	return nil
}
