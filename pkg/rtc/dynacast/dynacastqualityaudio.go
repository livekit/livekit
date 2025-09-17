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

package dynacast

import (
	"sync"

	"github.com/livekit/livekit-server/pkg/sfu/mime"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
)

var _ dynacastQuality = (*dynacastQualityAudio)(nil)

type dynacastQualityAudioParams struct {
	MimeType mime.MimeType
	Listener dynacastQualityListener
	Logger   logger.Logger
}

// dynacastQualityAudio manages enable a single receiver of a media track
type dynacastQualityAudio struct {
	params dynacastQualityAudioParams

	// quality level enable/disable
	lock                  sync.RWMutex
	initialized           bool
	subscriberEnables     map[livekit.ParticipantID]bool
	subscriberNodeEnables map[livekit.NodeID]bool
	enabled               bool
	regressTo             dynacastQuality

	dynacastQualityNull
}

func newDynacastQualityAudio(params dynacastQualityAudioParams) dynacastQuality {
	return &dynacastQualityAudio{
		params:                params,
		subscriberEnables:     make(map[livekit.ParticipantID]bool),
		subscriberNodeEnables: make(map[livekit.NodeID]bool),
	}
}

func (d *dynacastQualityAudio) Start() {
	d.reset()
}

func (d *dynacastQualityAudio) Restart() {
	d.reset()
}

func (d *dynacastQualityAudio) Stop() {
}

func (d *dynacastQualityAudio) NotifySubscription(subscriberID livekit.ParticipantID, enabled bool) {
	d.params.Logger.Debugw(
		"setting subscriber codec enable",
		"mime", d.params.MimeType,
		"subscriberID", subscriberID,
		"enabled", enabled,
	)

	d.lock.Lock()
	if r := d.regressTo; r != nil {
		d.lock.Unlock()
		return
	}

	if !enabled {
		delete(d.subscriberEnables, subscriberID)
	} else {
		d.subscriberEnables[subscriberID] = true
	}
	d.lock.Unlock()

	d.updateQualityChange(false)
}

func (d *dynacastQualityAudio) NotifySubscriptionNode(nodeID livekit.NodeID, enabled bool) {
	d.params.Logger.Debugw(
		"setting subscriber node codec enabled",
		"mime", d.params.MimeType,
		"subscriberNodeID", nodeID,
		"enabled", enabled,
	)

	d.lock.Lock()
	if r := d.regressTo; r != nil {
		// the downstream node will synthesize correct enable (its dynacast manager has codec regression), just ignore it
		d.params.Logger.Debugw(
			"ignoring node codec change, regressed to another dynacast quality",
			"mime", d.params.MimeType,
			"regressedMime", d.regressTo.Mime(),
		)
		d.lock.Unlock()
		return
	}

	if !enabled {
		delete(d.subscriberNodeEnables, nodeID)
	} else {
		d.subscriberNodeEnables[nodeID] = true
	}
	d.lock.Unlock()

	d.updateQualityChange(false)
}

func (d *dynacastQualityAudio) ClearSubscriberNodes() {
	d.lock.Lock()
	d.subscriberNodeEnables = make(map[livekit.NodeID]bool)
	d.lock.Unlock()

	d.updateQualityChange(false)
}

func (d *dynacastQualityAudio) Mime() mime.MimeType {
	return d.params.MimeType
}

func (d *dynacastQualityAudio) RegressTo(other dynacastQuality) {
	d.lock.Lock()
	d.regressTo = other
	d.lock.Unlock()

	other.Restart()
}

func (d *dynacastQualityAudio) reset() {
	d.lock.Lock()
	d.initialized = false
	d.lock.Unlock()
}

func (d *dynacastQualityAudio) updateQualityChange(force bool) {
	d.lock.Lock()
	enabled := len(d.subscriberEnables) != 0 || len(d.subscriberNodeEnables) != 0
	if enabled == d.enabled && d.initialized && !force {
		d.lock.Unlock()
		return
	}

	d.initialized = true
	d.enabled = enabled
	d.params.Logger.Debugw(
		"notifying enabled change",
		"mime", d.params.MimeType,
		"enabled", d.enabled,
		"subscriberNodeEnables", d.subscriberNodeEnables,
		"subscribedEnables", d.subscriberEnables,
		"force", force,
	)
	d.lock.Unlock()

	d.params.Listener.OnUpdateAudioCodecForMime(d.params.MimeType, enabled)
}
