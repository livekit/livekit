// Copyright 2024 LiveKit, Inc.
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

package sfu

import (
	"sync"
	"sync/atomic"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/sfu/rtpextension"
	"github.com/livekit/protocol/logger"
)

type PlayoutDelayState int32

const (
	PlayoutDelayStateChanged PlayoutDelayState = iota
	PlayoutDelaySending
	PlayoutDelayAcked

	jitterLowMultiToDelay  = 10
	jitterHighMultiToDelay = 15
	jitterHighThreshold    = 15

	targetDelayLogThreshold = 500
)

func (s PlayoutDelayState) String() string {
	switch s {
	case PlayoutDelayStateChanged:
		return "StateChanged"
	case PlayoutDelaySending:
		return "Sending"
	case PlayoutDelayAcked:
		return "Acked"
	}
	return "Unknown"
}

type PlayoutDelayController struct {
	lock               sync.Mutex
	state              atomic.Int32
	minDelay, maxDelay uint32
	currentDelay       uint32
	extBytes           atomic.Value //[]byte
	sendingAtSeq       uint16
	logger             logger.Logger
	rtpStats           *buffer.RTPStatsSender
	snapshotID         uint32

	highDelayCount atomic.Uint32
}

func NewPlayoutDelayController(minDelay, maxDelay uint32, logger logger.Logger, rtpStats *buffer.RTPStatsSender) (*PlayoutDelayController, error) {
	if maxDelay == 0 && minDelay > 0 {
		maxDelay = rtpextension.MaxPlayoutDelayDefault
	}
	if maxDelay > rtpextension.PlayoutDelayMaxValue {
		maxDelay = rtpextension.PlayoutDelayMaxValue
	}
	c := &PlayoutDelayController{
		currentDelay: minDelay,
		minDelay:     minDelay,
		maxDelay:     maxDelay,
		logger:       logger,
		rtpStats:     rtpStats,
		snapshotID:   rtpStats.NewSenderSnapshotId(),
	}
	return c, c.createExtData()
}

func (c *PlayoutDelayController) SetJitter(jitter uint32) {
	deltaInfo := c.rtpStats.DeltaInfoSender(c.snapshotID)
	var nackPercent uint32
	if deltaInfo != nil && deltaInfo.Packets > 0 {
		nackPercent = deltaInfo.Nacks * 100 / deltaInfo.Packets
	}

	c.lock.Lock()
	multi := jitterLowMultiToDelay
	if jitter >= jitterHighThreshold {
		multi = jitterHighMultiToDelay
	}
	targetDelay := jitter * uint32(multi)
	if nackPercent > 60 {
		targetDelay += (nackPercent - 60) * 2
	}

	// increase delay quickly, decrease slowly to make fps more stable
	if targetDelay > c.currentDelay {
		targetDelay = (targetDelay-c.currentDelay)*3/4 + c.currentDelay
	} else {
		targetDelay = c.currentDelay - (c.currentDelay-targetDelay)/5
	}
	if targetDelay < c.minDelay {
		targetDelay = c.minDelay
	}
	if targetDelay > c.maxDelay {
		targetDelay = c.maxDelay
	}
	if c.currentDelay == targetDelay {
		c.lock.Unlock()
		return
	}
	if targetDelay > targetDelayLogThreshold {
		if c.highDelayCount.Add(1)%100 == 1 {
			c.logger.Infow("high playout delay", "target", targetDelay, "jitter", jitter, "nackPercent", nackPercent, "current", c.currentDelay)
		}
	}
	c.currentDelay = targetDelay
	c.lock.Unlock()
	c.createExtData()
}

func (c *PlayoutDelayController) OnSeqAcked(seq uint16) {
	c.lock.Lock()
	defer c.lock.Unlock()
	if PlayoutDelayState(c.state.Load()) == PlayoutDelaySending && (seq-c.sendingAtSeq) < 0x8000 {
		c.state.Store(int32(PlayoutDelayAcked))
	}
}

func (c *PlayoutDelayController) GetDelayExtension(seq uint16) []byte {
	switch PlayoutDelayState(c.state.Load()) {
	case PlayoutDelayStateChanged:
		c.lock.Lock()
		c.state.Store(int32(PlayoutDelaySending))
		c.sendingAtSeq = seq
		c.lock.Unlock()
		return c.extBytes.Load().([]byte)
	case PlayoutDelaySending:
		return c.extBytes.Load().([]byte)
	case PlayoutDelayAcked:
		return nil
	}
	return nil
}

func (c *PlayoutDelayController) createExtData() error {
	delay := rtpextension.PlayoutDelayFromValue(
		uint16(c.currentDelay),
		uint16(c.maxDelay),
	)
	b, err := delay.Marshal()
	if err == nil {
		c.extBytes.Store(b)
		c.state.Store(int32(PlayoutDelayStateChanged))
	} else {
		c.logger.Errorw("failed to marshal playout delay", err, "playoutDelay", delay)
	}
	return err
}
