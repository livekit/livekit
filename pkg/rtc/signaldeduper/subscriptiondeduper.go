package signaldeduper

import (
	"sync"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/rtc/types"
)

// --------------------------------------------------

type subscriptionSetting struct {
	isEnabled bool
	quality   livekit.VideoQuality
	width     uint32
	height    uint32
	fps       uint32
}

func subscriptionSettingFromUpdateSubscription(us *livekit.UpdateSubscription) *subscriptionSetting {
	return &subscriptionSetting{
		isEnabled: us.Subscribe,
	}
}

func subscriptionSettingFromUpdateTrackSettings(uts *livekit.UpdateTrackSettings) *subscriptionSetting {
	return &subscriptionSetting{
		isEnabled: !uts.Disabled,
		quality:   uts.Quality,
		width:     uts.Width,
		height:    uts.Height,
		fps:       uts.Fps,
	}
}

func (s *subscriptionSetting) Equal(other *subscriptionSetting) bool {
	return s.isEnabled == other.isEnabled &&
		s.quality == other.quality &&
		s.width == other.width &&
		s.height == other.height &&
		s.fps == other.fps
}

// --------------------------------------------------

type SubscriptionDeduper struct {
	logger logger.Logger

	lock                      sync.RWMutex
	participantsSubscriptions map[livekit.ParticipantKey]map[livekit.TrackID]*subscriptionSetting
}

func NewSubscriptionDeduper(logger logger.Logger) types.SignalDeduper {
	return &SubscriptionDeduper{
		logger:                    logger,
		participantsSubscriptions: make(map[livekit.ParticipantKey]map[livekit.TrackID]*subscriptionSetting),
	}
}

func (s *SubscriptionDeduper) Dedupe(participantKey livekit.ParticipantKey, req *livekit.SignalRequest) bool {
	isDupe := false
	switch msg := req.Message.(type) {
	case *livekit.SignalRequest_Subscription:
		isDupe = s.updateSubscriptionsFromUpdateSubscription(participantKey, msg.Subscription)
	case *livekit.SignalRequest_TrackSetting:
		isDupe = s.updateSubscriptionsFromUpdateTrackSettings(participantKey, msg.TrackSetting)
	}

	return isDupe
}

func (s *SubscriptionDeduper) ParticipantClosed(participantKey livekit.ParticipantKey) {
	s.lock.Lock()
	defer s.lock.Unlock()

	delete(s.participantsSubscriptions, participantKey)
}

func (s *SubscriptionDeduper) updateSubscriptionsFromUpdateSubscription(
	participantKey livekit.ParticipantKey,
	us *livekit.UpdateSubscription,
) bool {
	isDupe := true

	s.lock.Lock()
	defer s.lock.Unlock()

	participantSubscriptions := s.getOrCreateParticipantSubscriptions(participantKey)

	numTracks := len(us.TrackSids)
	for _, pt := range us.ParticipantTracks {
		numTracks += len(pt.TrackSids)
	}
	trackIDs := make(map[livekit.TrackID]bool, numTracks)
	for _, trackSid := range us.TrackSids {
		trackIDs[livekit.TrackID(trackSid)] = true
	}
	for _, pt := range us.ParticipantTracks {
		for _, trackSid := range pt.TrackSids {
			trackIDs[livekit.TrackID(trackSid)] = true
		}
	}

	for trackID := range trackIDs {
		currentSetting := participantSubscriptions[trackID]
		if currentSetting == nil {
			// new track seen
			currentSetting = subscriptionSettingFromUpdateSubscription(us)
			participantSubscriptions[trackID] = currentSetting
			isDupe = false
		} else {
			newSetting := subscriptionSettingFromUpdateSubscription(us)
			if !currentSetting.Equal(newSetting) {
				// subscription setting change
				participantSubscriptions[trackID] = newSetting
				isDupe = false
			}
		}
	}

	return isDupe
}

func (s *SubscriptionDeduper) updateSubscriptionsFromUpdateTrackSettings(
	participantKey livekit.ParticipantKey,
	uts *livekit.UpdateTrackSettings,
) bool {
	isDupe := true

	s.lock.Lock()
	defer s.lock.Unlock()

	participantSubscriptions := s.getOrCreateParticipantSubscriptions(participantKey)

	for _, trackSid := range uts.TrackSids {
		currentSetting := participantSubscriptions[livekit.TrackID(trackSid)]
		if currentSetting == nil {
			// new track seen
			currentSetting = subscriptionSettingFromUpdateTrackSettings(uts)
			participantSubscriptions[livekit.TrackID(trackSid)] = currentSetting
			isDupe = false
		} else {
			newSetting := subscriptionSettingFromUpdateTrackSettings(uts)
			if !currentSetting.Equal(newSetting) {
				// subscription setting change
				participantSubscriptions[livekit.TrackID(trackSid)] = newSetting
				isDupe = false
			}
		}
	}

	return isDupe
}

func (s *SubscriptionDeduper) getOrCreateParticipantSubscriptions(participantKey livekit.ParticipantKey) map[livekit.TrackID]*subscriptionSetting {
	participantSubscriptions := s.participantsSubscriptions[participantKey]
	if participantSubscriptions == nil {
		participantSubscriptions = make(map[livekit.TrackID]*subscriptionSetting)
		s.participantsSubscriptions[participantKey] = participantSubscriptions
	}

	return participantSubscriptions
}
