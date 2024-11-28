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
	"errors"
	"fmt"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/psrpc"

	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/rtc/types"
)

func (p *ParticipantImpl) getResponseSink() routing.MessageSink {
	p.resSinkMu.Lock()
	defer p.resSinkMu.Unlock()
	return p.resSink
}

func (p *ParticipantImpl) SetResponseSink(sink routing.MessageSink) {
	p.resSinkMu.Lock()
	defer p.resSinkMu.Unlock()
	p.resSink = sink
}

func (p *ParticipantImpl) SendJoinResponse(joinResponse *livekit.JoinResponse) error {
	// keep track of participant updates and versions
	p.updateLock.Lock()
	for _, op := range joinResponse.OtherParticipants {
		p.updateCache.Add(livekit.ParticipantID(op.Sid), participantUpdateInfo{
			identity:  livekit.ParticipantIdentity(op.Identity),
			version:   op.Version,
			state:     op.State,
			updatedAt: time.Now(),
		})
	}
	p.updateLock.Unlock()

	// send Join response
	err := p.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_Join{
			Join: joinResponse,
		},
	})
	if err != nil {
		return err
	}

	// update state after sending message, so that no participant updates could slip through before JoinResponse is sent
	p.updateLock.Lock()
	if p.State() == livekit.ParticipantInfo_JOINING {
		p.updateState(livekit.ParticipantInfo_JOINED)
	}
	queuedUpdates := p.queuedUpdates
	p.queuedUpdates = nil
	p.updateLock.Unlock()

	if len(queuedUpdates) > 0 {
		return p.SendParticipantUpdate(queuedUpdates)
	}

	return nil
}

func (p *ParticipantImpl) SendParticipantUpdate(participantsToUpdate []*livekit.ParticipantInfo) error {
	p.updateLock.Lock()
	if p.IsDisconnected() {
		p.updateLock.Unlock()
		return nil
	}

	if !p.IsReady() {
		// queue up updates
		p.queuedUpdates = append(p.queuedUpdates, participantsToUpdate...)
		p.updateLock.Unlock()
		return nil
	}
	validUpdates := make([]*livekit.ParticipantInfo, 0, len(participantsToUpdate))
	for _, pi := range participantsToUpdate {
		isValid := true
		pID := livekit.ParticipantID(pi.Sid)
		if lastVersion, ok := p.updateCache.Get(pID); ok {
			// this is a message delivered out of order, a more recent version of the message had already been
			// sent.
			if pi.Version < lastVersion.version {
				p.params.Logger.Debugw(
					"skipping outdated participant update",
					"otherParticipant", pi.Identity,
					"otherPID", pi.Sid,
					"version", pi.Version,
					"lastVersion", lastVersion,
				)
				isValid = false
			}
		}
		if pi.Permission != nil && pi.Permission.Hidden && pi.Sid != string(p.params.SID) {
			p.params.Logger.Debugw("skipping hidden participant update", "otherParticipant", pi.Identity)
			isValid = false
		}
		if isValid {
			p.updateCache.Add(pID, participantUpdateInfo{
				identity:  livekit.ParticipantIdentity(pi.Identity),
				version:   pi.Version,
				state:     pi.State,
				updatedAt: time.Now(),
			})
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
func (p *ParticipantImpl) SendSpeakerUpdate(speakers []*livekit.SpeakerInfo, force bool) error {
	if !p.IsReady() {
		return nil
	}

	var scopedSpeakers []*livekit.SpeakerInfo
	if force {
		scopedSpeakers = speakers
	} else {
		for _, s := range speakers {
			participantID := livekit.ParticipantID(s.Sid)
			if p.IsSubscribedTo(participantID) || participantID == p.ID() {
				scopedSpeakers = append(scopedSpeakers, s)
			}
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

func (p *ParticipantImpl) SendRequestResponse(requestResponse *livekit.RequestResponse) error {
	if requestResponse.RequestId == 0 || !p.params.ClientInfo.SupportErrorResponse() {
		return nil
	}

	if requestResponse.Reason == livekit.RequestResponse_OK && !p.ProtocolVersion().SupportsNonErrorSignalResponse() {
		return nil
	}

	return p.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_RequestResponse{
			RequestResponse: requestResponse,
		},
	})
}

func (p *ParticipantImpl) HandleReconnectAndSendResponse(reconnectReason livekit.ReconnectReason, reconnectResponse *livekit.ReconnectResponse) error {
	p.TransportManager.HandleClientReconnect(reconnectReason)

	if !p.params.ClientInfo.CanHandleReconnectResponse() {
		return nil
	}
	if err := p.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_Reconnect{
			Reconnect: reconnectResponse,
		},
	}); err != nil {
		return err
	}

	if p.params.ProtocolVersion.SupportHandlesDisconnectedUpdate() {
		return p.sendDisconnectUpdatesForReconnect()
	}

	return nil
}

func (p *ParticipantImpl) sendDisconnectUpdatesForReconnect() error {
	lastSignalAt := p.TransportManager.LastSeenSignalAt()
	var disconnectedParticipants []*livekit.ParticipantInfo
	p.updateLock.Lock()
	keys := p.updateCache.Keys()
	for i := len(keys) - 1; i >= 0; i-- {
		if info, ok := p.updateCache.Get(keys[i]); ok {
			if info.updatedAt.Before(lastSignalAt) {
				break
			} else if info.state == livekit.ParticipantInfo_DISCONNECTED {
				disconnectedParticipants = append(disconnectedParticipants, &livekit.ParticipantInfo{
					Sid:      string(keys[i]),
					Identity: string(info.identity),
					Version:  info.version,
					State:    livekit.ParticipantInfo_DISCONNECTED,
				})
			}
		}
	}
	p.updateLock.Unlock()

	if len(disconnectedParticipants) == 0 {
		return nil
	}

	return p.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_Update{
			Update: &livekit.ParticipantUpdate{
				Participants: disconnectedParticipants,
			},
		},
	})
}

func (p *ParticipantImpl) sendICECandidate(ic *webrtc.ICECandidate, target livekit.SignalTarget) error {
	prevIC := p.icQueue[target].Swap(ic)
	if prevIC == nil {
		return nil
	}

	trickle := ToProtoTrickle(prevIC.ToJSON(), target, ic == nil)
	p.params.Logger.Debugw("sending ICE candidate", "transport", target, "trickle", logger.Proto(trickle))

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

func (p *ParticipantImpl) sendTrackHasBeenSubscribed(trackID livekit.TrackID) {
	if !p.params.ClientInfo.SupportTrackSubscribedEvent() {
		return
	}
	_ = p.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_TrackSubscribed{
			TrackSubscribed: &livekit.TrackSubscribed{
				TrackSid: string(trackID),
			},
		},
	})
	p.params.Logger.Debugw("track has been subscribed", "trackID", trackID)
}

func (p *ParticipantImpl) writeMessage(msg *livekit.SignalResponse) error {
	if p.IsDisconnected() || (!p.IsReady() && msg.GetJoin() == nil) {
		return nil
	}

	sink := p.getResponseSink()
	if sink == nil {
		p.params.Logger.Debugw("could not send message to participant", "messageType", fmt.Sprintf("%T", msg.Message))
		return nil
	}

	err := sink.WriteMessage(msg)
	if errors.Is(err, psrpc.Canceled) {
		p.params.Logger.Debugw("could not send message to participant",
			"error", err, "messageType", fmt.Sprintf("%T", msg.Message))
		return nil
	} else if err != nil {
		p.params.Logger.Warnw("could not send message to participant", err,
			"messageType", fmt.Sprintf("%T", msg.Message))
		return err
	}
	return nil
}

// closes signal connection to notify client to resume/reconnect
func (p *ParticipantImpl) CloseSignalConnection(reason types.SignallingCloseReason) {
	sink := p.getResponseSink()
	if sink != nil {
		p.params.Logger.Debugw("closing signal connection", "reason", reason, "connID", sink.ConnectionID())
		sink.Close()
		p.SetResponseSink(nil)
	}
}

func (p *ParticipantImpl) sendLeaveRequest(
	reason types.ParticipantCloseReason,
	isExpectedToResume bool,
	isExpectedToReconnect bool,
	sendOnlyIfSupportingLeaveRequestWithAction bool,
) error {
	var leave *livekit.LeaveRequest
	if p.ProtocolVersion().SupportsRegionsInLeaveRequest() {
		leave = &livekit.LeaveRequest{
			Reason: reason.ToDisconnectReason(),
		}
		switch {
		case isExpectedToResume:
			leave.Action = livekit.LeaveRequest_RESUME
		case isExpectedToReconnect:
			leave.Action = livekit.LeaveRequest_RECONNECT
		default:
			leave.Action = livekit.LeaveRequest_DISCONNECT
		}
		if leave.Action != livekit.LeaveRequest_DISCONNECT && p.params.GetRegionSettings != nil {
			// sending region settings even for RESUME just in case client wants to a full reconnect despite server saying RESUME
			leave.Regions = p.params.GetRegionSettings(p.params.ClientInfo.Address)
		}
	} else {
		if !sendOnlyIfSupportingLeaveRequestWithAction {
			leave = &livekit.LeaveRequest{
				CanReconnect: isExpectedToReconnect,
				Reason:       reason.ToDisconnectReason(),
			}
		}
	}
	if leave != nil {
		return p.writeMessage(&livekit.SignalResponse{
			Message: &livekit.SignalResponse_Leave{
				Leave: leave,
			},
		})
	}

	return nil
}
