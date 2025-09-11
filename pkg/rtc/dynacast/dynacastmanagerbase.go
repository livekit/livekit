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

	"golang.org/x/exp/maps"

	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/sfu/mime"
	"github.com/livekit/livekit-server/pkg/utils"
)

type dynacastManagerBaseParams struct {
	Logger                  logger.Logger
	OpsQueueDepth           uint
	OnRestart               func()
	OnDynacastQualityCreate func(mimeType mime.MimeType) dynacastQuality
	OnRegressCodec          func(fromMime, toMime mime.MimeType)
	OnUpdateNeeded          func(force bool)
}

type dynacastManagerBase struct {
	params dynacastManagerBaseParams

	lock            sync.RWMutex
	regressedCodec  map[mime.MimeType]struct{}
	dynacastQuality map[mime.MimeType]dynacastQuality

	notifyOpsQueue *utils.OpsQueue

	isClosed bool

	dynacastManagerNull
	dynacastQualityListenerNull
}

func newDynacastManagerBase(params dynacastManagerBaseParams) *dynacastManagerBase {
	if params.OpsQueueDepth == 0 {
		params.OpsQueueDepth = 4
	}
	d := &dynacastManagerBase{
		params:          params,
		regressedCodec:  make(map[mime.MimeType]struct{}),
		dynacastQuality: make(map[mime.MimeType]dynacastQuality),
		notifyOpsQueue: utils.NewOpsQueue(utils.OpsQueueParams{
			Name:        "dynacast-notify",
			MinSize:     params.OpsQueueDepth,
			FlushOnStop: true,
			Logger:      params.Logger,
		}),
	}
	d.notifyOpsQueue.Start()
	return d
}

func (d *dynacastManagerBase) AddCodec(mime mime.MimeType) {
	d.getOrCreateDynacastQuality(mime)
}

func (d *dynacastManagerBase) HandleCodecRegression(fromMime, toMime mime.MimeType) {
	fromDq := d.getOrCreateDynacastQuality(fromMime)

	d.lock.Lock()
	if d.isClosed {
		d.lock.Unlock()
		return
	}

	if fromDq == nil {
		// should not happen as we have added the codec on setup receiver
		d.params.Logger.Warnw("regression from codec not found", nil, "mime", fromMime, "toMime", toMime)
		d.lock.Unlock()
		return
	}
	d.regressedCodec[fromMime] = struct{}{}
	d.params.OnRegressCodec(fromMime, toMime)
	d.lock.Unlock()

	d.params.OnUpdateNeeded(false)

	fromDq.Stop()
	fromDq.RegressTo(d.getOrCreateDynacastQuality(toMime))
}

func (d *dynacastManagerBase) Restart() {
	d.lock.Lock()
	d.params.OnRestart()

	dqs := d.getDynacastQualitiesLocked()
	d.lock.Unlock()

	for _, dq := range dqs {
		dq.Restart()
	}
}

func (d *dynacastManagerBase) Close() {
	d.notifyOpsQueue.Stop()

	d.lock.Lock()
	dqs := d.getDynacastQualitiesLocked()
	d.dynacastQuality = make(map[mime.MimeType]dynacastQuality)

	d.isClosed = true
	d.lock.Unlock()

	for _, dq := range dqs {
		dq.Stop()
	}
}

// There are situations like track unmute or streaming from a different node
// where subscription changes needs to sent to the provider immediately.
// This bypasses any debouncing and forces a subscription change update
// with immediate effect.
func (d *dynacastManagerBase) ForceUpdate() {
	d.params.OnUpdateNeeded(true)
}

func (d *dynacastManagerBase) ClearSubscriberNodes() {
	d.lock.Lock()
	dqs := d.getDynacastQualitiesLocked()
	d.lock.Unlock()
	for _, dq := range dqs {
		dq.ClearSubscriberNodes()
	}
}

func (d *dynacastManagerBase) getOrCreateDynacastQuality(mimeType mime.MimeType) dynacastQuality {
	d.lock.Lock()
	defer d.lock.Unlock()

	if d.isClosed || mimeType == mime.MimeTypeUnknown {
		return nil
	}

	if dq := d.dynacastQuality[mimeType]; dq != nil {
		return dq
	}

	dq := d.params.OnDynacastQualityCreate(mimeType)
	dq.Start()

	d.dynacastQuality[mimeType] = dq
	return dq
}

func (d *dynacastManagerBase) getDynacastQualitiesLocked() []dynacastQuality {
	return maps.Values(d.dynacastQuality)
}
