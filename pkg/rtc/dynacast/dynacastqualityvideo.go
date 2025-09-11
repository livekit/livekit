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
	"time"

	"github.com/livekit/livekit-server/pkg/sfu/mime"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
)

var _ dynacastQuality = (*dynacastQualityVideo)(nil)

const (
	initialQualityUpdateWait = 10 * time.Second
)

type dynacastQualityVideoParams struct {
	MimeType mime.MimeType
	Listener dynacastQualityListener
	Logger   logger.Logger
}

// dynacastQualityVideo manages max subscribed quality of a single receiver of a media track
type dynacastQualityVideo struct {
	params dynacastQualityVideoParams

	// quality level enable/disable
	lock                     sync.RWMutex
	initialized              bool
	maxSubscriberQuality     map[livekit.ParticipantID]livekit.VideoQuality
	maxSubscriberNodeQuality map[livekit.NodeID]livekit.VideoQuality
	maxSubscribedQuality     livekit.VideoQuality
	maxQualityTimer          *time.Timer
	regressTo                dynacastQuality

	dynacastQualityNull
}

func newDynacastQualityVideo(params dynacastQualityVideoParams) dynacastQuality {
	return &dynacastQualityVideo{
		params:                   params,
		maxSubscriberQuality:     make(map[livekit.ParticipantID]livekit.VideoQuality),
		maxSubscriberNodeQuality: make(map[livekit.NodeID]livekit.VideoQuality),
	}
}

func (d *dynacastQualityVideo) Start() {
	d.reset()
}

func (d *dynacastQualityVideo) Restart() {
	d.reset()
}

func (d *dynacastQualityVideo) Stop() {
	d.stopMaxQualityTimer()
}

func (d *dynacastQualityVideo) NotifySubscriberMaxQuality(subscriberID livekit.ParticipantID, quality livekit.VideoQuality) {
	d.params.Logger.Debugw(
		"setting subscriber max quality",
		"mime", d.params.MimeType,
		"subscriberID", subscriberID,
		"quality", quality.String(),
	)

	d.lock.Lock()
	if r := d.regressTo; r != nil {
		d.lock.Unlock()
		r.NotifySubscriberMaxQuality(subscriberID, quality)
		return
	}

	if quality == livekit.VideoQuality_OFF {
		delete(d.maxSubscriberQuality, subscriberID)
	} else {
		d.maxSubscriberQuality[subscriberID] = quality
	}
	d.lock.Unlock()

	d.updateQualityChange(false)
}

func (d *dynacastQualityVideo) NotifySubscriberNodeMaxQuality(nodeID livekit.NodeID, quality livekit.VideoQuality) {
	d.params.Logger.Debugw(
		"setting subscriber node max quality",
		"mime", d.params.MimeType,
		"subscriberNodeID", nodeID,
		"quality", quality.String(),
	)

	d.lock.Lock()
	if r := d.regressTo; r != nil {
		// the downstream node will synthesize correct quality notify (its dynacast manager has codec regression), just ignore it
		d.params.Logger.Debugw(
			"ignoring node quality change, regressed to another dynacast quality",
			"mime", d.params.MimeType,
			"regressedMime", d.regressTo.Mime(),
		)
		d.lock.Unlock()
		return
	}

	if quality == livekit.VideoQuality_OFF {
		delete(d.maxSubscriberNodeQuality, nodeID)
	} else {
		d.maxSubscriberNodeQuality[nodeID] = quality
	}
	d.lock.Unlock()

	d.updateQualityChange(false)
}

func (d *dynacastQualityVideo) ClearSubscriberNodes() {
	d.lock.Lock()
	d.maxSubscriberNodeQuality = make(map[livekit.NodeID]livekit.VideoQuality)
	d.lock.Unlock()

	d.updateQualityChange(false)
}

func (d *dynacastQualityVideo) Mime() mime.MimeType {
	return d.params.MimeType
}

func (d *dynacastQualityVideo) RegressTo(other dynacastQuality) {
	d.lock.Lock()
	d.regressTo = other
	maxSubscriberQuality := d.maxSubscriberQuality
	maxSubscriberNodeQuality := d.maxSubscriberNodeQuality
	d.maxSubscriberQuality = make(map[livekit.ParticipantID]livekit.VideoQuality)
	d.maxSubscriberNodeQuality = make(map[livekit.NodeID]livekit.VideoQuality)
	d.lock.Unlock()

	other.Replace(maxSubscriberQuality, maxSubscriberNodeQuality)
}

func (d *dynacastQualityVideo) Replace(
	maxSubscriberQuality map[livekit.ParticipantID]livekit.VideoQuality,
	maxSubscriberNodeQuality map[livekit.NodeID]livekit.VideoQuality,
) {
	d.lock.Lock()
	for subID, quality := range maxSubscriberQuality {
		if oldQuality, ok := d.maxSubscriberQuality[subID]; ok {
			// no QUALITY_OFF in the map
			if quality > oldQuality {
				d.maxSubscriberQuality[subID] = quality
			}
		} else {
			d.maxSubscriberQuality[subID] = quality
		}
	}

	for nodeID, quality := range maxSubscriberNodeQuality {
		if oldQuality, ok := d.maxSubscriberNodeQuality[nodeID]; ok {
			// no QUALITY_OFF in the map
			if quality > oldQuality {
				d.maxSubscriberNodeQuality[nodeID] = quality
			}
		} else {
			d.maxSubscriberNodeQuality[nodeID] = quality
		}
	}
	d.lock.Unlock()

	d.Restart()
}

func (d *dynacastQualityVideo) reset() {
	d.lock.Lock()
	d.initialized = false
	d.lock.Unlock()

	d.startMaxQualityTimer()
}

func (d *dynacastQualityVideo) updateQualityChange(force bool) {
	d.lock.Lock()
	maxSubscribedQuality := livekit.VideoQuality_OFF
	for _, subQuality := range d.maxSubscriberQuality {
		if maxSubscribedQuality == livekit.VideoQuality_OFF || (subQuality != livekit.VideoQuality_OFF && subQuality > maxSubscribedQuality) {
			maxSubscribedQuality = subQuality
		}
	}
	for _, nodeQuality := range d.maxSubscriberNodeQuality {
		if maxSubscribedQuality == livekit.VideoQuality_OFF || (nodeQuality != livekit.VideoQuality_OFF && nodeQuality > maxSubscribedQuality) {
			maxSubscribedQuality = nodeQuality
		}
	}

	if maxSubscribedQuality == d.maxSubscribedQuality && d.initialized && !force {
		d.lock.Unlock()
		return
	}

	d.initialized = true
	d.maxSubscribedQuality = maxSubscribedQuality
	d.params.Logger.Debugw(
		"notifying quality change",
		"mime", d.params.MimeType,
		"maxSubscriberQuality", d.maxSubscriberQuality,
		"maxSubscriberNodeQuality", d.maxSubscriberNodeQuality,
		"maxSubscribedQuality", d.maxSubscribedQuality,
		"force", force,
	)
	d.lock.Unlock()

	d.params.Listener.OnUpdateMaxQualityForMime(d.params.MimeType, maxSubscribedQuality)
}

func (d *dynacastQualityVideo) startMaxQualityTimer() {
	d.lock.Lock()
	defer d.lock.Unlock()

	if d.maxQualityTimer != nil {
		d.maxQualityTimer.Stop()
		d.maxQualityTimer = nil
	}

	d.maxQualityTimer = time.AfterFunc(initialQualityUpdateWait, func() {
		d.stopMaxQualityTimer()
		d.updateQualityChange(true)
	})
}

func (d *dynacastQualityVideo) stopMaxQualityTimer() {
	d.lock.Lock()
	defer d.lock.Unlock()

	if d.maxQualityTimer != nil {
		d.maxQualityTimer.Stop()
		d.maxQualityTimer = nil
	}
}
