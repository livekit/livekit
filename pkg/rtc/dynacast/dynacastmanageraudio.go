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
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/sfu/mime"
)

var _ DynacastManager = (*dynacastManagerAudio)(nil)
var _ dynacastQualityListener = (*dynacastManagerAudio)(nil)

type DynacastManagerAudioParams struct {
	Listener DynacastManagerListener
	Logger   logger.Logger
}

type dynacastManagerAudio struct {
	params DynacastManagerAudioParams

	subscribedCodecs          map[mime.MimeType]bool
	committedSubscribedCodecs map[mime.MimeType]bool

	isClosed bool

	*dynacastManagerBase
}

func NewDynacastManagerAudio(params DynacastManagerAudioParams) DynacastManager {
	if params.Logger == nil {
		params.Logger = logger.GetLogger()
	}
	d := &dynacastManagerAudio{
		params:                    params,
		subscribedCodecs:          make(map[mime.MimeType]bool),
		committedSubscribedCodecs: make(map[mime.MimeType]bool),
	}
	d.dynacastManagerBase = newDynacastManagerBase(dynacastManagerBaseParams{
		Logger:        params.Logger,
		OpsQueueDepth: 4,
		OnRestart: func() {
			d.committedSubscribedCodecs = make(map[mime.MimeType]bool)
		},
		OnDynacastQualityCreate: func(mimeType mime.MimeType) dynacastQuality {
			dq := newDynacastQualityAudio(dynacastQualityAudioParams{
				MimeType: mimeType,
				Listener: d,
				Logger:   d.params.Logger,
			})
			return dq
		},
		OnRegressCodec: func(fromMime, toMime mime.MimeType) {
			d.subscribedCodecs[fromMime] = false

			// if the new codec is not added, notify the publisher to start publishing
			if _, ok := d.subscribedCodecs[toMime]; !ok {
				d.subscribedCodecs[toMime] = true
			}
		},
		OnUpdateNeeded: d.update,
	})
	return d
}

// It is possible for tracks to be in pending close state. When track
// is waiting to be closed, a node is not streaming a track. This can
// be used to force an update announcing that subscribed codec is disabled,
// i.e. indicating not pulling track any more.
func (d *dynacastManagerAudio) ForceEnable(enabled bool) {
	d.lock.Lock()
	defer d.lock.Unlock()

	for mime := range d.committedSubscribedCodecs {
		d.committedSubscribedCodecs[mime] = enabled
	}

	d.enqueueSubscribedChange()
}

func (d *dynacastManagerAudio) NotifySubscription(
	subscriberID livekit.ParticipantID,
	mime mime.MimeType,
	enabled bool,
) {
	dq := d.getOrCreateDynacastQuality(mime)
	if dq != nil {
		dq.NotifySubscription(subscriberID, enabled)
	}
}

func (d *dynacastManagerAudio) NotifySubscriptionNode(
	nodeID livekit.NodeID,
	codecs []*livekit.SubscribedAudioCodec,
) {
	for _, codec := range codecs {
		dq := d.getOrCreateDynacastQuality(mime.NormalizeMimeType(codec.Codec))
		if dq != nil {
			dq.NotifySubscriptionNode(nodeID, codec.Enabled)
		}
	}
}

func (d *dynacastManagerAudio) OnUpdateAudioCodecForMime(mime mime.MimeType, enabled bool) {
	d.lock.Lock()
	if _, ok := d.regressedCodec[mime]; !ok {
		d.subscribedCodecs[mime] = enabled
	}
	d.lock.Unlock()

	d.update(false)
}

func (d *dynacastManagerAudio) update(force bool) {
	d.lock.Lock()

	d.params.Logger.Debugw(
		"processing subscribed codec change",
		"force", force,
		"committedSubscribedCodecs", d.committedSubscribedCodecs,
		"subscribedCodecs", d.subscribedCodecs,
	)

	if len(d.subscribedCodecs) == 0 {
		// no mime has been added, nothing to update
		d.lock.Unlock()
		return
	}

	// add or remove of a mime triggers an update
	changed := len(d.subscribedCodecs) != len(d.committedSubscribedCodecs)
	if !changed {
		for mime, enabled := range d.subscribedCodecs {
			if ce, ok := d.committedSubscribedCodecs[mime]; ok {
				if ce != enabled {
					changed = true
					break
				}
			}
		}
	}

	if !force && !changed {
		d.lock.Unlock()
		return
	}

	d.params.Logger.Debugw(
		"committing subscribed codec change",
		"force", force,
		"committedSubscribedCoecs", d.committedSubscribedCodecs,
		"subscribedcodecs", d.subscribedCodecs,
	)

	// commit change
	d.committedSubscribedCodecs = make(map[mime.MimeType]bool, len(d.subscribedCodecs))
	for mime, enabled := range d.subscribedCodecs {
		d.committedSubscribedCodecs[mime] = enabled
	}

	d.enqueueSubscribedChange()
	d.lock.Unlock()
}

func (d *dynacastManagerAudio) enqueueSubscribedChange() {
	if d.isClosed || d.params.Listener == nil {
		return
	}

	subscribedCodecs := make([]*livekit.SubscribedAudioCodec, 0, len(d.committedSubscribedCodecs))
	for mime, enabled := range d.committedSubscribedCodecs {
		subscribedCodecs = append(subscribedCodecs, &livekit.SubscribedAudioCodec{
			Codec:   mime.String(),
			Enabled: enabled,
		})
	}

	d.params.Logger.Debugw(
		"subscribedAudioCodecChange",
		"subscribedCodecs", logger.ProtoSlice(subscribedCodecs),
	)
	d.notifyOpsQueue.Enqueue(func() {
		d.params.Listener.OnDynacastSubscribedAudioCodecChange(subscribedCodecs)
	})
}
