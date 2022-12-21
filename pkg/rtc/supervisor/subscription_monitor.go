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
	transitionWaitDuration = 20 * time.Second
)

var (
	errSubscribeTimeout   = errors.New("subscribe time out")
	errUnsubscribeTimeout = errors.New("unsubscribe time out")
)

type transition struct {
	isSubscribe bool
	at          time.Time
}

type subscriptionOps struct {
	desiredTransitions deque.Deque

	subscribedTrack types.SubscribedTrack
}

type SubscriptionOpParams struct {
	SourceTrack types.MediaTrack
	IsSubscribe bool
}

type UpdateSubscribedTrackParams struct {
	SourceTrack     types.MediaTrack
	SubscribedTrack types.SubscribedTrack
}

type SubscriptionMonitorParams struct {
	TrackID livekit.TrackID
	Logger  logger.Logger
}

type SubscriptionMonitor struct {
	params SubscriptionMonitorParams

	lock                    sync.RWMutex
	subscriptionOpsBySource map[types.MediaTrack]*subscriptionOps
}

func NewSubscriptionMonitor(params SubscriptionMonitorParams) *SubscriptionMonitor {
	s := &SubscriptionMonitor{
		params:                  params,
		subscriptionOpsBySource: make(map[types.MediaTrack]*subscriptionOps),
	}
	return s
}

func (s *SubscriptionMonitor) PostEvent(ome types.OperationMonitorEvent, omd types.OperationMonitorData) {
	switch ome {
	case types.OperationMonitorEventUpdateSubscription:
		s.updateSubscription(omd.(SubscriptionOpParams))
	case types.OperationMonitorEventSetSubscribedTrack:
		s.setSubscribedTrack(omd.(UpdateSubscribedTrackParams))
	case types.OperationMonitorEventClearSubscribedTrack:
		s.clearSubscribedTrack(omd.(UpdateSubscribedTrackParams))
	}
}

func (s *SubscriptionMonitor) updateSubscription(params SubscriptionOpParams) {
	s.lock.Lock()

	so := s.getOrCreateSubscriptionOpsForSource(params.SourceTrack)
	so.desiredTransitions.PushBack(
		&transition{
			isSubscribe: params.IsSubscribe,
			at:          time.Now(),
		},
	)
	s.update()
	s.lock.Unlock()
}

func (s *SubscriptionMonitor) setSubscribedTrack(params UpdateSubscribedTrackParams) {
	s.lock.Lock()
	so := s.getOrCreateSubscriptionOpsForSource(params.SourceTrack)
	so.subscribedTrack = params.SubscribedTrack
	s.update()
	s.lock.Unlock()
}

func (s *SubscriptionMonitor) clearSubscribedTrack(params UpdateSubscribedTrackParams) {
	s.lock.Lock()
	so := s.getOrCreateSubscriptionOpsForSource(params.SourceTrack)
	if so.subscribedTrack == params.SubscribedTrack {
		so.subscribedTrack = nil
	} else {
		s.params.Logger.Errorw("supervisor: mismatched subscribed track on clear", nil, "trackID", s.params.TrackID)
	}

	s.update()
	s.lock.Unlock()
}

func (s *SubscriptionMonitor) Check() error {
	s.lock.Lock()
	defer s.lock.Unlock()

	for _, so := range s.subscriptionOpsBySource {
		var tx *transition
		if so.desiredTransitions.Len() > 0 {
			tx = so.desiredTransitions.Front().(*transition)
		}

		if tx == nil {
			continue
		}

		if time.Since(tx.at) > transitionWaitDuration {
			// timed out waiting for transition
			if tx.isSubscribe {
				return errSubscribeTimeout
			} else {
				return errUnsubscribeTimeout
			}
		}
	}

	// give more time for transition to happen
	return nil
}

func (s *SubscriptionMonitor) IsIdle() bool {
	s.lock.RLock()
	defer s.lock.RUnlock()

	return len(s.subscriptionOpsBySource) == 0
}

func (s *SubscriptionMonitor) getOrCreateSubscriptionOpsForSource(sourceTrack types.MediaTrack) *subscriptionOps {
	so := s.subscriptionOpsBySource[sourceTrack]
	if so == nil {
		so = &subscriptionOps{}
		so.desiredTransitions.SetMinCapacity(4)
		s.subscriptionOpsBySource[sourceTrack] = so
	}

	return so
}

func (s *SubscriptionMonitor) update() {
	var toReap []types.MediaTrack
	for sourceTrack, so := range s.subscriptionOpsBySource {
		for {
			var tx *transition
			if so.desiredTransitions.Len() > 0 {
				tx = so.desiredTransitions.PopFront().(*transition)
			}

			if tx == nil {
				break
			}

			if (tx.isSubscribe && so.subscribedTrack == nil) || (!tx.isSubscribe && so.subscribedTrack != nil) {
				// put it back as the condition is not satisfied
				so.desiredTransitions.PushFront(tx)
				break
			}

			if so.desiredTransitions.Len() == 0 && so.subscribedTrack == nil {
				toReap = append(toReap, sourceTrack)
			}
		}
	}

	for _, st := range toReap {
		delete(s.subscriptionOpsBySource, st)
	}
}
