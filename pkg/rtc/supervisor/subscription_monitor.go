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
	errSubscribeTimeout   = errors.New("subscribe time out")
	errUnsubscribeTimeout = errors.New("unsubscribe time out")
)

type transition struct {
	isSubscribe bool
	at          time.Time
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

	lastError error
}

func NewSubscriptionMonitor(params SubscriptionMonitorParams) *SubscriptionMonitor {
	s := &SubscriptionMonitor{
		params: params,
	}
	s.desiredTransitions.SetMinCapacity(2)
	return s
}

func (s *SubscriptionMonitor) PostEvent(ome types.OperationMonitorEvent, omd types.OperationMonitorData) {
	switch ome {
	case types.OperationMonitorEventUpdateSubscription:
		s.updateSubscription(omd.(bool))
	case types.OperationMonitorEventSetSubscribedTrack:
		s.setSubscribedTrack(omd.(types.SubscribedTrack))
	case types.OperationMonitorEventClearSubscribedTrack:
		s.clearSubscribedTrack(omd.(types.SubscribedTrack))
	}
}

func (s *SubscriptionMonitor) updateSubscription(isSubscribe bool) {
	s.lock.Lock()
	s.desiredTransitions.PushBack(
		&transition{
			isSubscribe: isSubscribe,
			at:          time.Now(),
		},
	)
	s.update()
	s.lock.Unlock()
}

func (s *SubscriptionMonitor) setSubscribedTrack(subTrack types.SubscribedTrack) {
	s.lock.Lock()
	s.subscribedTrack = subTrack
	s.update()
	s.lock.Unlock()
}

func (s *SubscriptionMonitor) clearSubscribedTrack(subTrack types.SubscribedTrack) {
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
	if s.lastError != nil {
		s.lock.RUnlock()
		// return an error only once
		return nil
	}

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
		var err error
		if tx.isSubscribe {
			err = errSubscribeTimeout
		} else {
			err = errUnsubscribeTimeout
		}

		s.lock.Lock()
		s.lastError = err
		s.lock.Unlock()

		return err
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
	for {
		var tx *transition
		if s.desiredTransitions.Len() > 0 {
			tx = s.desiredTransitions.PopFront().(*transition)
		}

		if tx == nil {
			return
		}

		if (tx.isSubscribe && s.subscribedTrack == nil) || (!tx.isSubscribe && s.subscribedTrack != nil) {
			// put it back as the condition is not satisfied
			s.desiredTransitions.PushFront(tx)
			return
		}

		s.lastError = nil
	}
}
