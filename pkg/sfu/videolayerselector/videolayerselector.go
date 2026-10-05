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

package videolayerselector

import (
	"errors"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	dd "github.com/livekit/livekit-server/pkg/sfu/rtpextension/dependencydescriptor"
	"github.com/livekit/livekit-server/pkg/sfu/videolayerselector/temporallayerselector"
	"github.com/livekit/protocol/logger"
)

type VideoLayerSelectorResult struct {
	IsSelected  bool
	IsRelevant  bool
	IsSwitching bool
	IsResuming  bool
	RTPMarker   bool

	// marshaled dependency descriptor extension, held inline when it fits and
	// spilled to the heap when it does not, the way the sequencer keeps its copy
	DDBytes      [dd.MaxInlineExtensionSize]byte
	DDBytesLen   int
	DDBytesSpill []byte
}

// marshalDependencyDescriptorExtension marshals ddExtension into the result,
// inline if it fits and on the heap otherwise.
func (v *VideoLayerSelectorResult) marshalDependencyDescriptorExtension(ddExtension *dd.DependencyDescriptorExtension) error {
	ddBytesLen, err := ddExtension.MarshalTo(v.DDBytes[:])
	if err == nil {
		v.DDBytesLen = ddBytesLen
		return nil
	}
	if !errors.Is(err, dd.ErrBufferTooSmall) {
		return err
	}

	// descriptors carrying the full dependency structure do not fit inline
	v.DDBytesSpill, err = ddExtension.Marshal()
	return err
}

type VideoLayerSelector interface {
	getBase() *Base

	getLogger() logger.Logger

	IsOvershootOkay() bool

	SetTemporalLayerSelector(tls temporallayerselector.TemporalLayerSelector)
	SetEnableStartAtDesiredQuality(enable bool)

	SetMax(maxLayer buffer.VideoLayer)
	SetMaxSpatial(layer int32)
	SetMaxTemporal(layer int32)
	GetMax() buffer.VideoLayer

	SetTarget(targetLayer buffer.VideoLayer)
	GetTarget() buffer.VideoLayer

	SetRequestSpatial(layer int32)
	GetRequestSpatial() int32

	CheckSync() (locked bool, layer int32)

	SetMaxSeen(maxSeenLayer buffer.VideoLayer)
	SetMaxSeenSpatial(layer int32)
	SetMaxSeenTemporal(layer int32)
	GetMaxSeen() buffer.VideoLayer

	SetCurrent(currentLayer buffer.VideoLayer)
	GetCurrent() buffer.VideoLayer

	Select(extPkt *buffer.ExtPacket, layer int32) VideoLayerSelectorResult
	SelectTemporal(extPkt *buffer.ExtPacket) int32
	Rollback()
}
