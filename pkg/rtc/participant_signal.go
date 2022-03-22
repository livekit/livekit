package rtc

import (
	"fmt"

	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/version"
	"github.com/livekit/protocol/livekit"
	"github.com/pion/webrtc/v3"
	"google.golang.org/protobuf/proto"
)

func (p *ParticipantImpl) GetResponseSink() routing.MessageSink {
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

func (p *ParticipantImpl) SendJoinResponse(
	roomInfo *livekit.Room,
	otherParticipants []*livekit.ParticipantInfo,
	iceServers []*livekit.ICEServer,
	region string,
) error {
	// send Join response
	return p.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_Join{
			Join: &livekit.JoinResponse{
				Room:              roomInfo,
				Participant:       p.ToProto(),
				OtherParticipants: otherParticipants,
				ServerVersion:     version.Version,
				ServerRegion:      region,
				IceServers:        iceServers,
				// indicates both server and client support subscriber as primary
				SubscriberPrimary:   p.SubscriberAsPrimary(),
				ClientConfiguration: p.params.ClientConf,
			},
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
		if p.isSubscribedTo(participantID) || participantID == p.ID() {
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

	data, err := proto.Marshal(dp)
	if err != nil {
		return err
	}

	var dc *webrtc.DataChannel
	if dp.Kind == livekit.DataPacket_RELIABLE {
		if p.SubscriberAsPrimary() {
			dc = p.reliableDCSub
		} else {
			dc = p.reliableDC
		}
	} else {
		if p.SubscriberAsPrimary() {
			dc = p.lossyDCSub
		} else {
			dc = p.lossyDC
		}
	}

	if dc == nil {
		return ErrDataChannelUnavailable
	}
	return dc.Send(data)
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

func (p *ParticipantImpl) sendIceCandidate(c *webrtc.ICECandidate, target livekit.SignalTarget) {
	ci := c.ToJSON()

	// write candidate
	p.params.Logger.Debugw("sending ice candidates",
		"candidate", c.String(), "target", target)
	ci.Candidate += fmt.Sprintf(" network-cost %d", p.networkCost)
	trickle := ToProtoTrickle(ci)
	trickle.Target = target
	_ = p.writeMessage(&livekit.SignalResponse{
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
	sink := p.GetResponseSink()
	if sink == nil {
		return nil
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
func (p *ParticipantImpl) closeSignalConnection() {
	sink := p.GetResponseSink()
	if sink != nil {
		sink.Close()
		p.SetResponseSink(nil)
	}
}
