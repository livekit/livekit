package rtc

import (
	"fmt"

	"github.com/pion/webrtc/v3"

	"github.com/livekit/protocol/livekit"

	"github.com/livekit/livekit-server/pkg/routing"
)

func (p *ParticipantImpl) getResponseSink() routing.MessageSink {
	if !p.resSinkValid.Load() {
		return nil
	}
	sink := p.resSink.Load()
	if s, ok := sink.(routing.MessageSink); ok {
		return s
	}
	return nil
}

func (p *ParticipantImpl) SetResponseSink(sink routing.MessageSink) {
	p.resSinkValid.Store(sink != nil)
	if sink != nil {
		// cannot store nil into atomic.Value
		p.resSink.Store(sink)
	}
}

func (p *ParticipantImpl) SendJoinResponse(joinResponse *livekit.JoinResponse) error {
	if p.State() == livekit.ParticipantInfo_JOINING {
		p.updateState(livekit.ParticipantInfo_JOINED)
	}

	// send Join response
	return p.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_Join{
			Join: joinResponse,
		},
	})
}

func (p *ParticipantImpl) SendParticipantUpdate(participantsToUpdate []*livekit.ParticipantInfo) error {
	p.updateLock.Lock()
	validUpdates := make([]*livekit.ParticipantInfo, 0, len(participantsToUpdate))
	for _, pi := range participantsToUpdate {
		isValid := true
		if val, ok := p.updateCache.Get(pi.Sid); ok {
			if lastVersion, ok := val.(uint32); ok {
				// this is a message delivered out of order, a more recent version of the message had already been
				// sent.
				if pi.Version < lastVersion {
					p.params.Logger.Debugw("skipping outdated participant update", "version", pi.Version, "lastVersion", lastVersion)
					isValid = false
				}
			}
		}
		if isValid {
			p.updateCache.Add(pi.Sid, pi.Version)
			validUpdates = append(validUpdates, pi)
		}
	}
	p.updateLock.Unlock()

	if len(validUpdates) == 0 {
		return nil
	}

	return p.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_Update{
			Update: &livekit.ParticipantUpdate{
				Participants: validUpdates,
			},
		},
	})
}

// SendSpeakerUpdate notifies participant changes to speakers. only send members that have changed since last update
func (p *ParticipantImpl) SendSpeakerUpdate(speakers []*livekit.SpeakerInfo) error {
	if !p.IsReady() {
		return nil
	}

	var scopedSpeakers []*livekit.SpeakerInfo
	for _, s := range speakers {
		participantID := livekit.ParticipantID(s.Sid)
		if p.IsSubscribedTo(participantID) || participantID == p.ID() {
			scopedSpeakers = append(scopedSpeakers, s)
		}
	}

	if len(scopedSpeakers) == 0 {
		return nil
	}

	return p.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_SpeakersChanged{
			SpeakersChanged: &livekit.SpeakersChanged{
				Speakers: scopedSpeakers,
			},
		},
	})
}

func (p *ParticipantImpl) SendDataPacket(dp *livekit.DataPacket) error {
	if p.State() != livekit.ParticipantInfo_ACTIVE {
		return ErrDataChannelUnavailable
	}

	return p.TransportManager.SendDataPacket(dp)
}

func (p *ParticipantImpl) SendRoomUpdate(room *livekit.Room) error {
	return p.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_RoomUpdate{
			RoomUpdate: &livekit.RoomUpdate{
				Room: room,
			},
		},
	})
}

func (p *ParticipantImpl) SendConnectionQualityUpdate(update *livekit.ConnectionQualityUpdate) error {
	return p.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_ConnectionQuality{
			ConnectionQuality: update,
		},
	})
}

func (p *ParticipantImpl) SendRefreshToken(token string) error {
	return p.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_RefreshToken{
			RefreshToken: token,
		},
	})
}

func (p *ParticipantImpl) sendICECandidate(c *webrtc.ICECandidate, target livekit.SignalTarget) error {
	trickle := ToProtoTrickle(c.ToJSON())
	trickle.Target = target
	return p.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_Trickle{
			Trickle: trickle,
		},
	})
}

func (p *ParticipantImpl) sendTrackMuted(trackID livekit.TrackID, muted bool) {
	_ = p.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_Mute{
			Mute: &livekit.MuteTrackRequest{
				Sid:   string(trackID),
				Muted: muted,
			},
		},
	})
}

func (p *ParticipantImpl) sendTrackUnpublished(trackID livekit.TrackID) {
	_ = p.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_TrackUnpublished{
			TrackUnpublished: &livekit.TrackUnpublishedResponse{
				TrackSid: string(trackID),
			},
		},
	})
}

func (p *ParticipantImpl) writeMessage(msg *livekit.SignalResponse) error {
	if p.State() == livekit.ParticipantInfo_DISCONNECTED {
		return nil
	}

	sink := p.getResponseSink()
	if sink == nil {
		err := fmt.Errorf("no response sink")
		p.params.Logger.Warnw("could not send message to participant", err,
			"message", fmt.Sprintf("%T", msg.Message))
		return err
	}

	err := sink.WriteMessage(msg)
	if err != nil {
		p.params.Logger.Warnw("could not send message to participant", err,
			"message", fmt.Sprintf("%T", msg.Message))
		return err
	}
	return nil
}

// closes signal connection to notify client to resume/reconnect
func (p *ParticipantImpl) CloseSignalConnection() {
	sink := p.getResponseSink()
	if sink != nil {
		p.params.Logger.Infow("closing signal connection")
		sink.Close()
		p.SetResponseSink(nil)
	}
}
