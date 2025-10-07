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

	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/sfu/mime"
)

type DynacastManagerListener interface {
	OnDynacastSubscribedMaxQualityChange(
		subscribedQualities []*livekit.SubscribedCodec,
		maxSubscribedQualities []types.SubscribedCodecQuality,
	)

	OnDynacastSubscribedAudioCodecChange(codecs []*livekit.SubscribedAudioCodec)
}

var _ DynacastManagerListener = (*DynacastManagerListenerNull)(nil)

type DynacastManagerListenerNull struct {
}

func (d *DynacastManagerListenerNull) OnDynacastSubscribedMaxQualityChange(
	subscribedQualities []*livekit.SubscribedCodec,
	maxSubscribedQualities []types.SubscribedCodecQuality,
) {
}
func (d *DynacastManagerListenerNull) OnDynacastSubscribedAudioCodecChange(
	codecs []*livekit.SubscribedAudioCodec,
) {
}

// -----------------------------------------

type DynacastManager interface {
	AddCodec(mime mime.MimeType)
	HandleCodecRegression(fromMime, toMime mime.MimeType)
	Restart()
	Close()
	ForceUpdate()
	ForceQuality(quality livekit.VideoQuality)
	ForceEnable(enabled bool)

	NotifySubscriberMaxQuality(
		subscriberID livekit.ParticipantID,
		mime mime.MimeType,
		quality livekit.VideoQuality,
	)
	NotifySubscription(
		subscriberID livekit.ParticipantID,
		mime mime.MimeType,
		enabled bool,
	)

	NotifySubscriberNodeMaxQuality(
		nodeID livekit.NodeID,
		qualities []types.SubscribedCodecQuality,
	)
	NotifySubscriptionNode(
		nodeID livekit.NodeID,
		codecs []*livekit.SubscribedAudioCodec,
	)
	ClearSubscriberNodes()
}

var _ DynacastManager = (*dynacastManagerNull)(nil)

type dynacastManagerNull struct {
}

func (d *dynacastManagerNull) AddCodec(mime mime.MimeType)                          {}
func (d *dynacastManagerNull) HandleCodecRegression(fromMime, toMime mime.MimeType) {}
func (d *dynacastManagerNull) Restart()                                             {}
func (d *dynacastManagerNull) Close()                                               {}
func (d *dynacastManagerNull) ForceUpdate()                                         {}
func (d *dynacastManagerNull) ForceQuality(quality livekit.VideoQuality)            {}
func (d *dynacastManagerNull) ForceEnable(enabled bool)                             {}
func (d *dynacastManagerNull) NotifySubscriberMaxQuality(
	subscriberID livekit.ParticipantID,
	mime mime.MimeType,
	quality livekit.VideoQuality,
) {
}
func (d *dynacastManagerNull) NotifySubscription(
	subscriberID livekit.ParticipantID,
	mime mime.MimeType,
	enabled bool,
) {
}
func (d *dynacastManagerNull) NotifySubscriberNodeMaxQuality(
	nodeID livekit.NodeID,
	qualities []types.SubscribedCodecQuality,
) {
}
func (d *dynacastManagerNull) NotifySubscriptionNode(
	nodeID livekit.NodeID,
	codecs []*livekit.SubscribedAudioCodec,
) {
}
func (d *dynacastManagerNull) ClearSubscriberNodes() {}

// ------------------------------------------------

type dynacastQualityListener interface {
	OnUpdateMaxQualityForMime(mimeType mime.MimeType, maxQuality livekit.VideoQuality)
	OnUpdateAudioCodecForMime(mimeType mime.MimeType, enabled bool)
}

var _ dynacastQualityListener = (*dynacastQualityListenerNull)(nil)

type dynacastQualityListenerNull struct {
}

func (d *dynacastQualityListenerNull) OnUpdateMaxQualityForMime(
	mimeType mime.MimeType,
	maxQuality livekit.VideoQuality,
) {
}

func (d *dynacastQualityListenerNull) OnUpdateAudioCodecForMime(
	mimeType mime.MimeType,
	enabled bool,
) {
}

// ------------------------------------------------

type dynacastQuality interface {
	Start()
	Restart()
	Stop()

	NotifySubscriberMaxQuality(subscriberID livekit.ParticipantID, quality livekit.VideoQuality)
	NotifySubscription(subscriberID livekit.ParticipantID, enabled bool)

	NotifySubscriberNodeMaxQuality(nodeID livekit.NodeID, quality livekit.VideoQuality)
	NotifySubscriptionNode(nodeID livekit.NodeID, enabled bool)
	ClearSubscriberNodes()

	Replace(
		maxSubscriberQuality map[livekit.ParticipantID]livekit.VideoQuality,
		maxSubscriberNodeQuality map[livekit.NodeID]livekit.VideoQuality,
	)

	Mime() mime.MimeType
	RegressTo(other dynacastQuality)
}

var _ dynacastQuality = (*dynacastQualityNull)(nil)

type dynacastQualityNull struct {
}

func (d *dynacastQualityNull) Start()   {}
func (d *dynacastQualityNull) Restart() {}
func (d *dynacastQualityNull) Stop()    {}
func (d *dynacastQualityNull) NotifySubscriberMaxQuality(subscriberID livekit.ParticipantID, quality livekit.VideoQuality) {
}
func (d *dynacastQualityNull) NotifySubscription(subscriberID livekit.ParticipantID, enabled bool) {}
func (d *dynacastQualityNull) NotifySubscriberNodeMaxQuality(nodeID livekit.NodeID, quality livekit.VideoQuality) {
}
func (d *dynacastQualityNull) NotifySubscriptionNode(nodeID livekit.NodeID, enabled bool) {}
func (d *dynacastQualityNull) ClearSubscriberNodes()                                      {}
func (d *dynacastQualityNull) Replace(
	maxSubscriberQuality map[livekit.ParticipantID]livekit.VideoQuality,
	maxSubscriberNodeQuality map[livekit.NodeID]livekit.VideoQuality,
) {
}
func (d *dynacastQualityNull) Mime() mime.MimeType             { return mime.MimeTypeUnknown }
func (d *dynacastQualityNull) RegressTo(other dynacastQuality) {}
