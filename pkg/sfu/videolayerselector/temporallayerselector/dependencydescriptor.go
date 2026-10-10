// Copyright 2026 LiveKit, Inc.
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

package temporallayerselector

import (
	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	dd "github.com/livekit/livekit-server/pkg/sfu/rtpextension/dependencydescriptor"
	"github.com/livekit/protocol/logger"
)

// frames tracked behind the highest frame number, more than temporal references reach
const ddFrameWindow = 64

// DependencyDescriptor switches temporal layers with the dependency descriptor alone, for
// a stream with one spatial layer. It only picks the layer, the codec munger drops the
// packets above it. libwebrtc strips the VP8 payload descriptor when it sends the
// dependency descriptor, so for VP8 the temporal ID and the layer sync bit are only here.
type DependencyDescriptor struct {
	logger logger.Logger

	initialized   bool
	highestFrame  uint64
	forwardedMask uint64 // bit i: frame highestFrame - i was forwarded
}

func NewDependencyDescriptor(logger logger.Logger) *DependencyDescriptor {
	return &DependencyDescriptor{
		logger: logger,
	}
}

func (d *DependencyDescriptor) Select(extPkt *buffer.ExtPacket, current int32, target int32) (this int32, next int32) {
	this = current
	next = current

	ddwdt := extPkt.DependencyDescriptor
	if ddwdt == nil || ddwdt.Descriptor == nil || ddwdt.Descriptor.FrameDependencies == nil {
		return
	}

	// A late first packet, a retransmission for example, starts a frame whose other
	// packets were already decided. A key frame is the exception: it references nothing,
	// and it restarts the frame numbers on a stream restart.
	isFrameStart := ddwdt.Descriptor.FirstPacketInFrame && !extPkt.IsOutOfOrder
	switch {
	case current < target && extPkt.IsKeyFrame:
		this = target
		next = target

	case current < target && isFrameStart:
		// A switch point of a decode target makes all later frames of that target
		// decodable if this frame is decodable, so also every frame this one references
		// has to have been forwarded.
		if !d.areForwarded(ddwdt.ExtFrameNum, ddwdt.Descriptor.FrameDependencies.FrameDiffs) {
			break
		}
		dtis := ddwdt.Descriptor.FrameDependencies.DecodeTargetIndications
		for _, dt := range ddwdt.DecodeTargets { // sorted from high to low
			if dt.Layer.Temporal <= current || dt.Layer.Temporal > target || dt.Target >= len(dtis) {
				continue
			}
			if dtis[dt.Target] == dd.DecodeTargetSwitch {
				this = dt.Layer.Temporal
				next = dt.Layer.Temporal
				break
			}
		}

	case current > target && extPkt.Packet.Marker:
		next = target
	}

	if extPkt.IsKeyFrame || isFrameStart {
		d.onFrame(ddwdt.ExtFrameNum, extPkt.IsKeyFrame, extPkt.Temporal <= this)
	}
	return
}

func (d *DependencyDescriptor) onFrame(frame uint64, isKeyFrame bool, isForwarded bool) {
	switch {
	case isKeyFrame || !d.initialized || frame >= d.highestFrame+ddFrameWindow:
		d.initialized = true
		d.highestFrame = frame
		d.forwardedMask = 0

	case frame > d.highestFrame:
		d.forwardedMask <<= frame - d.highestFrame
		d.highestFrame = frame

	case d.highestFrame-frame >= ddFrameWindow:
		return
	}

	if isForwarded {
		d.forwardedMask |= 1 << (d.highestFrame - frame)
	}
}

func (d *DependencyDescriptor) areForwarded(frame uint64, frameDiffs []int) bool {
	if !d.initialized {
		return false
	}
	for _, diff := range frameDiffs {
		if diff <= 0 || uint64(diff) > frame {
			return false
		}
		ref := frame - uint64(diff)
		if ref > d.highestFrame || d.highestFrame-ref >= ddFrameWindow || d.forwardedMask&(1<<(d.highestFrame-ref)) == 0 {
			return false
		}
	}
	return true
}
