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
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	protosignalling "github.com/livekit/protocol/signalling"

	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/rtc/types"
)

func (p *ParticipantImpl) SetResponseSink(sink routing.MessageSink) {
	p.signaller.SetResponseSink(sink)
}

func (p *ParticipantImpl) GetResponseSink() routing.MessageSink {
	return p.signaller.GetResponseSink()
}

func (p *ParticipantImpl) CloseSignalConnection(reason types.SignallingCloseReason) {
	p.signaller.CloseSignalConnection(reason)
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
	err := p.signaller.WriteMessage(p.signalling.SignalJoinResponse(joinResponse))
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
		if pi.Permission != nil && pi.Permission.Hidden && pi.Sid != string(p.ID()) {
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

	return p.signaller.WriteMessage(p.signalling.SignalParticipantUpdate(validUpdates))
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

	return p.signaller.WriteMessage(p.signalling.SignalSpeakerUpdate(scopedSpeakers))
}

func (p *ParticipantImpl) SendRoomUpdate(room *livekit.Room) error {
	return p.signaller.WriteMessage(p.signalling.SignalRoomUpdate(room))
}

func (p *ParticipantImpl) SendConnectionQualityUpdate(update *livekit.ConnectionQualityUpdate) error {
	return p.signaller.WriteMessage(p.signalling.SignalConnectionQualityUpdate(update))
}

func (p *ParticipantImpl) SendRefreshToken(token string) error {
	return p.signaller.WriteMessage(p.signalling.SignalRefreshToken(token))
}

func (p *ParticipantImpl) sendRequestResponse(requestResponse *livekit.RequestResponse) error {
	if !p.params.ClientInfo.SupportsRequestResponse() {
		return nil
	}

	if requestResponse.Reason == livekit.RequestResponse_OK && !p.ProtocolVersion().SupportsNonErrorSignalResponse() {
		return nil
	}

	return p.signaller.WriteMessage(p.signalling.SignalRequestResponse(requestResponse))
}

func (p *ParticipantImpl) SendRoomMovedResponse(roomMovedResponse *livekit.RoomMovedResponse) error {
	return p.signaller.WriteMessage(p.signalling.SignalRoomMovedResponse(roomMovedResponse))
}

func (p *ParticipantImpl) HandleReconnectAndSendResponse(reconnectReason livekit.ReconnectReason, reconnectResponse *livekit.ReconnectResponse) error {
	p.TransportManager.HandleClientReconnect(reconnectReason)

	if !p.params.ClientInfo.CanHandleReconnectResponse() {
		return nil
	}
	if err := p.signaller.WriteMessage(p.signalling.SignalReconnectResponse(reconnectResponse)); err != nil {
		return err
	}

	if p.params.ProtocolVersion.SupportsDisconnectedUpdate() {
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

	return p.signaller.WriteMessage(p.signalling.SignalParticipantUpdate(disconnectedParticipants))
}

func (p *ParticipantImpl) sendICECandidate(ic *webrtc.ICECandidate, target livekit.SignalTarget) error {
	prevIC := p.icQueue[target].Swap(ic)
	if prevIC == nil {
		return nil
	}

	trickle := protosignalling.ToProtoTrickle(prevIC.ToJSON(), target, ic == nil)
	p.params.Logger.Debugw("sending ICE candidate", "transport", target, "trickle", logger.Proto(trickle))

	return p.signaller.WriteMessage(p.signalling.SignalICECandidate(trickle))
}

func (p *ParticipantImpl) sendTrackMuted(trackID livekit.TrackID, muted bool) {
	_ = p.signaller.WriteMessage(p.signalling.SignalTrackMuted(&livekit.MuteTrackRequest{
		Sid:   string(trackID),
		Muted: muted,
	}))
}

func (p *ParticipantImpl) sendTrackPublished(cid string, ti *livekit.TrackInfo) error {
	p.pubLogger.Debugw("sending track published", "cid", cid, "trackInfo", logger.Proto(ti))
	return p.signaller.WriteMessage(p.signalling.SignalTrackPublished(&livekit.TrackPublishedResponse{
		Cid:   cid,
		Track: ti,
	}))
}

func (p *ParticipantImpl) sendTrackUnpublished(trackID livekit.TrackID) {
	_ = p.signaller.WriteMessage(p.signalling.SignalTrackUnpublished(&livekit.TrackUnpublishedResponse{
		TrackSid: string(trackID),
	}))
}

func (p *ParticipantImpl) sendTrackHasBeenSubscribed(trackID livekit.TrackID) {
	if !p.params.ClientInfo.SupportsTrackSubscribedEvent() {
		return
	}
	_ = p.signaller.WriteMessage(p.signalling.SignalTrackSubscribed(&livekit.TrackSubscribed{
		TrackSid: string(trackID),
	}))
	p.params.Logger.Debugw("track has been subscribed", "trackID", trackID)
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
		if leave.Action != livekit.LeaveRequest_DISCONNECT {
			// sending region settings even for RESUME just in case client wants to a full reconnect despite server saying RESUME
			leave.Regions = p.helper().GetRegionSettings(p.params.ClientInfo.Address)
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
		return p.signaller.WriteMessage(p.signalling.SignalLeaveRequest(leave))
	}

	return nil
}

func (p *ParticipantImpl) sendSdpAnswer(answer webrtc.SessionDescription, answerId uint32) error {
	return p.signaller.WriteMessage(p.signalling.SignalSdpAnswer(protosignalling.ToProtoSessionDescription(answer, answerId)))
}

func (p *ParticipantImpl) sendMappedSdpAnswer(answer webrtc.SessionDescription, answerId uint32, midToTrackID map[string]string) error {
	return p.signaller.WriteMessage(p.signalling.SignalMappedSdpAnswer(protosignalling.ToProtoMappedSessionDescription(answer, answerId, midToTrackID)))
}

func (p *ParticipantImpl) sendSdpOffer(offer webrtc.SessionDescription, offerId uint32) error {
	return p.signaller.WriteMessage(p.signalling.SignalSdpOffer(protosignalling.ToProtoSessionDescription(offer, offerId)))
}

func (p *ParticipantImpl) sendStreamStateUpdate(streamStateUpdate *livekit.StreamStateUpdate) error {
	return p.signaller.WriteMessage(p.signalling.SignalStreamStateUpdate(streamStateUpdate))
}

func (p *ParticipantImpl) sendSubscribedQualityUpdate(subscribedQualityUpdate *livekit.SubscribedQualityUpdate) error {
	return p.signaller.WriteMessage(p.signalling.SignalSubscribedQualityUpdate(subscribedQualityUpdate))
}

func (p *ParticipantImpl) sendSubscribedAudioCodecUpdate(subscribedAudioCodecUpdate *livekit.SubscribedAudioCodecUpdate) error {
	return p.signaller.WriteMessage(p.signalling.SignalSubscribedAudioCodecUpdate(subscribedAudioCodecUpdate))
}

func (p *ParticipantImpl) sendSubscriptionResponse(trackID livekit.TrackID, subErr livekit.SubscriptionError) error {
	return p.signaller.WriteMessage(p.signalling.SignalSubscriptionResponse(&livekit.SubscriptionResponse{
		TrackSid: string(trackID),
		Err:      subErr,
	}))
}

func (p *ParticipantImpl) SendSubscriptionPermissionUpdate(publisherID livekit.ParticipantID, trackID livekit.TrackID, allowed bool) error {
	p.subLogger.Debugw("sending subscription permission update", "publisherID", publisherID, "trackID", trackID, "allowed", allowed)
	err := p.signaller.WriteMessage(p.signalling.SignalSubscriptionPermissionUpdate(&livekit.SubscriptionPermissionUpdate{
		ParticipantSid: string(publisherID),
		TrackSid:       string(trackID),
		Allowed:        allowed,
	}))
	if err != nil {
		p.subLogger.Errorw("could not send subscription permission update", err)
	}
	return err
}

func (p *ParticipantImpl) sendMediaSectionsRequirement(numAudios uint32, numVideos uint32) error {
	p.pubLogger.Debugw(
		"sending media sections requirement",
		"numAudios", numAudios,
		"numVideos", numVideos,
	)
	err := p.signaller.WriteMessage(p.signalling.SignalMediaSectionsRequirement(&livekit.MediaSectionsRequirement{
		NumAudios: numAudios,
		NumVideos: numVideos,
	}))
	if err != nil {
		p.subLogger.Errorw("could not send media sections requirement", err)
	}
	return err
}
