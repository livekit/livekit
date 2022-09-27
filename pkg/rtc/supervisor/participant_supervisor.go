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

	lock                 sync.RWMutex
	isPublisherConnected bool
	publications         map[livekit.TrackID]types.OperationMonitor
	subscriptions        map[livekit.TrackID]types.OperationMonitor

	isStopped atomic.Bool
}

func NewParticipantSupervisor(params ParticipantSupervisorParams) *ParticipantSupervisor {
	p := &ParticipantSupervisor{
		params:        params,
		publications:  make(map[livekit.TrackID]types.OperationMonitor),
		subscriptions: make(map[livekit.TrackID]types.OperationMonitor),
	}

	go p.checkState()

	return p
}

func (p *ParticipantSupervisor) Stop() {
	p.isStopped.Store(true)
}

func (p *ParticipantSupervisor) SetPublisherPeerConnectionConnected(isConnected bool) {
	p.lock.Lock()
	p.isPublisherConnected = isConnected

	for _, pm := range p.publications {
		pm.PostEvent(types.OperationMonitorEventPublisherPeerConnectionConnected, p.isPublisherConnected)
	}
	p.lock.Unlock()
}

func (p *ParticipantSupervisor) AddPublication(trackID livekit.TrackID) {
	p.lock.Lock()
	pm, ok := p.publications[trackID]
	if !ok {
		pm = NewPublicationMonitor(
			PublicationMonitorParams{
				TrackID:                   trackID,
				IsPeerConnectionConnected: p.isPublisherConnected,
				Logger:                    p.params.Logger,
			},
		)
		p.publications[trackID] = pm
	}
	pm.PostEvent(types.OperationMonitorEventAddPendingPublication, nil)
	p.lock.Unlock()
}

func (p *ParticipantSupervisor) SetPublicationMute(trackID livekit.TrackID, isMuted bool) {
	p.lock.Lock()
	pm, ok := p.publications[trackID]
	if ok {
		pm.PostEvent(types.OperationMonitorEventSetPublicationMute, isMuted)
	}
	p.lock.Unlock()
}

func (p *ParticipantSupervisor) SetPublishedTrack(trackID livekit.TrackID, pubTrack types.LocalMediaTrack) {
	p.lock.RLock()
	pm, ok := p.publications[trackID]
	if ok {
		pm.PostEvent(types.OperationMonitorEventSetPublishedTrack, pubTrack)
	}
	p.lock.RUnlock()
}

func (p *ParticipantSupervisor) ClearPublishedTrack(trackID livekit.TrackID, pubTrack types.LocalMediaTrack) {
	p.lock.RLock()
	pm, ok := p.publications[trackID]
	if ok {
		pm.PostEvent(types.OperationMonitorEventClearPublishedTrack, pubTrack)
	}
	p.lock.RUnlock()
}

func (p *ParticipantSupervisor) UpdateSubscription(trackID livekit.TrackID, isSubscribed bool) {
	p.lock.Lock()
	sm, ok := p.subscriptions[trackID]
	if !ok {
		sm = NewSubscriptionMonitor(SubscriptionMonitorParams{TrackID: trackID, Logger: p.params.Logger})
		p.subscriptions[trackID] = sm
	}
	sm.PostEvent(types.OperationMonitorEventUpdateSubscription, isSubscribed)
	p.lock.Unlock()
}

func (p *ParticipantSupervisor) SetSubscribedTrack(trackID livekit.TrackID, subTrack types.SubscribedTrack) {
	p.lock.RLock()
	sm, ok := p.subscriptions[trackID]
	if ok {
		sm.PostEvent(types.OperationMonitorEventSetSubscribedTrack, subTrack)
	}
	p.lock.RUnlock()
}

func (p *ParticipantSupervisor) ClearSubscribedTrack(trackID livekit.TrackID, subTrack types.SubscribedTrack) {
	p.lock.RLock()
	sm, ok := p.subscriptions[trackID]
	if ok {
		sm.PostEvent(types.OperationMonitorEventClearSubscribedTrack, subTrack)
	}
	p.lock.RUnlock()
}

func (p *ParticipantSupervisor) checkState() {
	ticker := time.NewTicker(monitorInterval)
	defer ticker.Stop()

	for !p.isStopped.Load() {
		<-ticker.C

		p.checkPublications()
		p.checkSubscriptions()
	}
}

func (p *ParticipantSupervisor) checkPublications() {
	var removablePublications []livekit.TrackID
	p.lock.RLock()
	for trackID, pm := range p.publications {
		if err := pm.Check(); err != nil {
			p.params.Logger.Errorw("supervisor error on publication", err, "trackID", trackID)
		} else {
			if pm.IsIdle() {
				removablePublications = append(removablePublications, trackID)
			}
		}
	}
	p.lock.RUnlock()

	p.lock.Lock()
	for _, trackID := range removablePublications {
		delete(p.publications, trackID)
	}
	p.lock.Unlock()
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
