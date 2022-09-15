package supervisor

import (
	"errors"
	"sync"
	"time"

	"github.com/gammazero/deque"
	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
)

const (
	transitionWaitDuration = 10 * time.Second
)

var (
	errTransitionTimeout = errors.New("transition time out")
)

type transition struct {
	isSubscribed bool
	at           time.Time
}

type SubscriptionMonitorParams struct {
	TrackID livekit.TrackID
	Logger  logger.Logger
}

type SubscriptionMonitor struct {
	params SubscriptionMonitorParams

	lock               sync.RWMutex
	desiredTransitions deque.Deque

	subscribedTrack types.SubscribedTrack
}

func NewSubscriptionMonitor(params SubscriptionMonitorParams) *SubscriptionMonitor {
	s := &SubscriptionMonitor{
		params: params,
	}
	s.desiredTransitions.SetMinCapacity(2)
	return s
}

func (s *SubscriptionMonitor) UpdateSubscription(isSubscribed bool) {
	s.lock.Lock()
	s.desiredTransitions.PushBack(
		&transition{
			isSubscribed: isSubscribed,
			at:           time.Now(),
		},
	)
	s.update()
	s.lock.Unlock()
}

func (s *SubscriptionMonitor) SetSubscribedTrack(subTrack types.SubscribedTrack) {
	s.lock.Lock()
	s.subscribedTrack = subTrack
	s.update()
	s.lock.Unlock()
}

func (s *SubscriptionMonitor) ClearSubscribedTrack(subTrack types.SubscribedTrack) {
	s.lock.Lock()
	if s.subscribedTrack == subTrack {
		s.subscribedTrack = nil
	} else {
		s.params.Logger.Errorw("mismatched subscribed track on clear", nil, "trackID", s.params.TrackID)
	}

	s.update()
	s.lock.Unlock()
}

func (s *SubscriptionMonitor) Check() error {
	s.lock.RLock()
	var tx *transition
	if s.desiredTransitions.Len() > 0 {
		tx = s.desiredTransitions.Front().(*transition)
	}
	s.lock.RUnlock()

	if tx == nil {
		return nil
	}

	if time.Since(tx.at) > transitionWaitDuration {
		// timed out waiting for transition
		return errTransitionTimeout
	}

	// give more time for transition to happen
	return nil
}

func (s *SubscriptionMonitor) IsIdle() bool {
	s.lock.RLock()
	defer s.lock.RUnlock()

	return s.desiredTransitions.Len() == 0 && s.subscribedTrack == nil
}

func (s *SubscriptionMonitor) update() {
	var tx *transition
	if s.desiredTransitions.Len() > 0 {
		tx = s.desiredTransitions.PopFront().(*transition)
	}

	if tx == nil {
		return
	}

	switch {
	case tx.isSubscribed && s.subscribedTrack != nil:
		return
	case !tx.isSubscribed && s.subscribedTrack == nil:
		return
	default:
		// put it back as the condition is not satisfied
		s.desiredTransitions.PushFront(tx)
		return
	}
}
