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
	desiredPublishes deque.Deque[*publish]

	isConnected bool

	publishedTrack types.LocalMediaTrack
	isMuted        bool
	unmutedAt      time.Time
}

func NewPublicationMonitor(params PublicationMonitorParams) *PublicationMonitor {
	p := &PublicationMonitor{
		params:      params,
		isConnected: params.IsPeerConnectionConnected,
	}
	p.desiredPublishes.SetBaseCap(4)
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
		p.params.Logger.Errorw("supervisor: mismatched published track on clear", nil, "trackID", p.params.TrackID)
	}

	p.update()
	p.lock.Unlock()
}

func (p *PublicationMonitor) Check() error {
	p.lock.RLock()
	var pub *publish
	if p.desiredPublishes.Len() > 0 {
		pub = p.desiredPublishes.Front()
	}

	isMuted := p.isMuted
	unmutedAt := p.unmutedAt
	p.lock.RUnlock()

	if pub == nil {
		return nil
	}

	if pub.isStart && !isMuted && !unmutedAt.IsZero() && time.Since(unmutedAt) > publishWaitDuration {
		// timed out waiting for publish
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
			pub = p.desiredPublishes.PopFront()
		}

		if pub == nil {
			return
		}

		if (pub.isStart && p.publishedTrack == nil) || (!pub.isStart && p.publishedTrack != nil) {
			// put it back as the condition is not satisfied
			p.desiredPublishes.PushFront(pub)
			return
		}
	}
}
