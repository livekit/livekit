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
	publishWaitDuration = 30 * time.Second
)

var (
	errPublishTimeout = errors.New("publish time out")
)

type publish struct {
	isStart bool
}

type PublicationMonitorParams struct {
	TrackID                   livekit.TrackID
	IsPeerConnectionConnected bool
	Logger                    logger.Logger
}

type PublicationMonitor struct {
	params PublicationMonitorParams

	lock             sync.RWMutex
	desiredPublishes deque.Deque

	isConnected bool

	publishedTrack types.LocalMediaTrack
	isMuted        bool
	unmutedAt      time.Time

	lastError error
}

func NewPublicationMonitor(params PublicationMonitorParams) *PublicationMonitor {
	p := &PublicationMonitor{
		params:      params,
		isConnected: params.IsPeerConnectionConnected,
	}
	p.desiredPublishes.SetMinCapacity(2)
	return p
}

func (p *PublicationMonitor) PostEvent(ome types.OperationMonitorEvent, omd types.OperationMonitorData) {
	switch ome {
	case types.OperationMonitorEventPublisherPeerConnectionConnected:
		p.setConnected(omd.(bool))
	case types.OperationMonitorEventAddPendingPublication:
		p.addPending()
	case types.OperationMonitorEventSetPublicationMute:
		p.setMute(omd.(bool))
	case types.OperationMonitorEventSetPublishedTrack:
		p.setPublishedTrack(omd.(types.LocalMediaTrack))
	case types.OperationMonitorEventClearPublishedTrack:
		p.clearPublishedTrack(omd.(types.LocalMediaTrack))
	}
}

func (p *PublicationMonitor) addPending() {
	p.lock.Lock()
	p.desiredPublishes.PushBack(
		&publish{
			isStart: true,
		},
	)

	// synthesize an end
	p.desiredPublishes.PushBack(
		&publish{
			isStart: false,
		},
	)
	p.update()
	p.lock.Unlock()
}

func (p *PublicationMonitor) maybeStartMonitor() {
	if p.isConnected && !p.isMuted {
		p.unmutedAt = time.Now()
	}
}

func (p *PublicationMonitor) setConnected(isConnected bool) {
	p.lock.Lock()
	p.isConnected = isConnected
	p.maybeStartMonitor()
	p.lock.Unlock()
}

func (p *PublicationMonitor) setMute(isMuted bool) {
	p.lock.Lock()
	p.isMuted = isMuted
	p.maybeStartMonitor()
	p.lock.Unlock()
}

func (p *PublicationMonitor) setPublishedTrack(pubTrack types.LocalMediaTrack) {
	p.lock.Lock()
	p.publishedTrack = pubTrack
	p.update()
	p.lock.Unlock()
}

func (p *PublicationMonitor) clearPublishedTrack(pubTrack types.LocalMediaTrack) {
	p.lock.Lock()
	if p.publishedTrack == pubTrack {
		p.publishedTrack = nil
	} else {
		p.params.Logger.Errorw("mismatched published track on clear", nil, "trackID", p.params.TrackID)
	}

	p.update()
	p.lock.Unlock()
}

func (p *PublicationMonitor) Check() error {
	p.lock.RLock()
	if p.lastError != nil {
		p.lock.RUnlock()
		// return an error only once
		return nil
	}

	var pub *publish
	if p.desiredPublishes.Len() > 0 {
		pub = p.desiredPublishes.Front().(*publish)
	}

	isMuted := p.isMuted
	unmutedAt := p.unmutedAt
	p.lock.RUnlock()

	if pub == nil {
		return nil
	}

	if pub.isStart && !isMuted && !unmutedAt.IsZero() && time.Since(unmutedAt) > publishWaitDuration {
		// timed out waiting for publish
		p.lock.Lock()
		p.lastError = errPublishTimeout
		p.lock.Unlock()

		return errPublishTimeout
	}

	// give more time for publish to happen
	// NOTE: synthesized end events do not have a start time, so do not check them for time out
	return nil
}

func (p *PublicationMonitor) IsIdle() bool {
	p.lock.RLock()
	defer p.lock.RUnlock()

	return p.desiredPublishes.Len() == 0 && p.publishedTrack == nil
}

func (p *PublicationMonitor) update() {
	for {
		var pub *publish
		if p.desiredPublishes.Len() > 0 {
			pub = p.desiredPublishes.PopFront().(*publish)
		}

		if pub == nil {
			return
		}

		if (pub.isStart && p.publishedTrack == nil) || (!pub.isStart && p.publishedTrack != nil) {
			// put it back as the condition is not satisfied
			p.desiredPublishes.PushFront(pub)
			return
		}

		p.lastError = nil
	}
}
