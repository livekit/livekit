package supervisor

import (
	"sync"
	"time"

	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"go.uber.org/atomic"
)

const (
	monitorInterval = 3 * time.Second
)

type ParticipantSupervisorParams struct {
	Logger logger.Logger
}

type ParticipantSupervisor struct {
	params ParticipantSupervisorParams

	lock          sync.RWMutex
	subscriptions map[livekit.TrackID]types.OperationMonitor

	isStopped atomic.Bool
}

func NewParticipantSupervisor(params ParticipantSupervisorParams) *ParticipantSupervisor {
	p := &ParticipantSupervisor{
		params:        params,
		subscriptions: make(map[livekit.TrackID]types.OperationMonitor),
	}

	go p.checkState()

	return p
}

func (p *ParticipantSupervisor) Stop() {
	p.isStopped.Store(true)
}

func (p *ParticipantSupervisor) UpdateSubscription(trackID livekit.TrackID, isSubscribed bool) {
	p.lock.Lock()
	sm, ok := p.subscriptions[trackID]
	if !ok {
		sm = NewSubscriptionMonitor(SubscriptionMonitorParams{TrackID: trackID, Logger: p.params.Logger})
		p.subscriptions[trackID] = sm
	}
	sm.(*SubscriptionMonitor).UpdateSubscription(isSubscribed)
	p.lock.Unlock()
}

func (p *ParticipantSupervisor) SetSubscribedTrack(trackID livekit.TrackID, subTrack types.SubscribedTrack) {
	p.lock.RLock()
	sm, ok := p.subscriptions[trackID]
	if ok {
		sm.(*SubscriptionMonitor).SetSubscribedTrack(subTrack)
	}
	p.lock.RUnlock()
}

func (p *ParticipantSupervisor) ClearSubscribedTrack(trackID livekit.TrackID, subTrack types.SubscribedTrack) {
	p.lock.RLock()
	sm, ok := p.subscriptions[trackID]
	if ok {
		sm.(*SubscriptionMonitor).ClearSubscribedTrack(subTrack)
	}
	p.lock.RUnlock()
}

func (p *ParticipantSupervisor) checkState() {
	ticker := time.NewTicker(monitorInterval)
	defer ticker.Stop()

	for !p.isStopped.Load() {
		<-ticker.C

		p.checkSubscriptions()
	}
}

func (p *ParticipantSupervisor) checkSubscriptions() {
	var removableSubscriptions []livekit.TrackID
	p.lock.RLock()
	for trackID, sm := range p.subscriptions {
		if err := sm.Check(); err != nil {
			p.params.Logger.Errorw("supervisor error on subscription", err, "trackID", trackID)
		} else {
			if sm.IsIdle() {
				removableSubscriptions = append(removableSubscriptions, trackID)
			}
		}
	}
	p.lock.RUnlock()

	p.lock.Lock()
	for _, trackID := range removableSubscriptions {
		delete(p.subscriptions, trackID)
	}
	p.lock.Unlock()
}
