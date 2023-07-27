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
	"sync"
	"time"

	"github.com/pion/rtcp"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/sfu"
)

const (
	downLostUpdateDelta = time.Second
)

type MediaLossProxyParams struct {
	Logger logger.Logger
}

type MediaLossProxy struct {
	params MediaLossProxyParams

	lock                 sync.Mutex
	maxDownFracLost      uint8
	maxDownFracLostTs    time.Time
	maxDownFracLostValid bool

	onMediaLossUpdate func(fractionalLoss uint8)
}

func NewMediaLossProxy(params MediaLossProxyParams) *MediaLossProxy {
	return &MediaLossProxy{params: params}
}

func (m *MediaLossProxy) OnMediaLossUpdate(f func(fractionalLoss uint8)) {
	m.lock.Lock()
	m.onMediaLossUpdate = f
	m.lock.Unlock()
}

func (m *MediaLossProxy) HandleMaxLossFeedback(_ *sfu.DownTrack, report *rtcp.ReceiverReport) {
	m.lock.Lock()
	for _, rr := range report.Reports {
		m.maxDownFracLostValid = true
		if m.maxDownFracLost < rr.FractionLost {
			m.maxDownFracLost = rr.FractionLost
		}
	}
	m.lock.Unlock()

	m.maybeUpdateLoss()
}

func (m *MediaLossProxy) NotifySubscriberNodeMediaLoss(_nodeID livekit.NodeID, fractionalLoss uint8) {
	m.lock.Lock()
	m.maxDownFracLostValid = true
	if m.maxDownFracLost < fractionalLoss {
		m.maxDownFracLost = fractionalLoss
	}
	m.lock.Unlock()

	m.maybeUpdateLoss()
}

func (m *MediaLossProxy) maybeUpdateLoss() {
	var (
		shouldUpdate bool
		maxLost      uint8
	)

	m.lock.Lock()
	now := time.Now()
	if now.Sub(m.maxDownFracLostTs) > downLostUpdateDelta && m.maxDownFracLostValid {
		shouldUpdate = true
		maxLost = m.maxDownFracLost
		m.maxDownFracLost = 0
		m.maxDownFracLostTs = now
		m.maxDownFracLostValid = false
	}
	onMediaLossUpdate := m.onMediaLossUpdate
	m.lock.Unlock()

	if shouldUpdate {
		if onMediaLossUpdate != nil {
			onMediaLossUpdate(maxLost)
		}
	}
}
