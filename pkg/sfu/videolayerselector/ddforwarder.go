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

package videolayerselector

import (
	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	dd "github.com/livekit/livekit-server/pkg/sfu/rtpextension/dependencydescriptor"
	"github.com/livekit/protocol/logger"
)

// DDForwarder forwards the dependency descriptor of a simulcast stream. Every simulcast
// layer has its own dependency structure and frame numbers, so at a layer switch the frame
// numbers continue from the last forwarded frame, and the structure of the new layer goes
// out with its key frame. Without the descriptor, a subscriber of a stream without picture
// IDs, VP8 from libwebrtc for example, finds frame references by sequence number alone and
// stalls on every sequence number it cannot account for.
type DDForwarder struct {
	logger logger.Logger

	initialized       bool
	ssrc              uint32
	restartGeneration int
	structure         *dd.FrameDependencyStructure
	frameNumOffset    uint64 // added to the incoming extended frame number
	extLastFrameNum   uint64 // highest outgoing extended frame number

	descriptor                 dd.DependencyDescriptor
	activeDecodeTargetsBitmask uint32
}

func NewDDForwarder(logger logger.Logger) *DDForwarder {
	return &DDForwarder{
		logger: logger,
	}
}

// Forward marshals the descriptor of a forwarded packet into result. temporal is the
// highest temporal layer forwarded. It returns false when there is nothing to send, before
// the first key frame of the stream for example.
func (d *DDForwarder) Forward(extPkt *buffer.ExtPacket, temporal int32, result *VideoLayerSelectorResult) bool {
	ddwdt := extPkt.DependencyDescriptor
	if ddwdt == nil || ddwdt.Descriptor == nil || ddwdt.Descriptor.FrameDependencies == nil {
		return false
	}
	descriptor := ddwdt.Descriptor

	extFrameNum := ddwdt.ExtFrameNum
	if extPkt.IsKeyFrame && descriptor.AttachedStructure != nil {
		switch {
		case !d.initialized:
			// keep the publisher's frame numbers, a migrated subscriber continues with them
			d.initialized = true
			d.frameNumOffset = 0
			d.extLastFrameNum = extFrameNum

		case extPkt.Packet.SSRC != d.ssrc || ddwdt.RestartGeneration != d.restartGeneration:
			// a new layer or a restarted stream has its own frame numbers, a late packet of
			// a forwarded key frame keeps its number
			d.frameNumOffset = d.extLastFrameNum + 1 - extFrameNum
		}
		d.ssrc = extPkt.Packet.SSRC
		d.restartGeneration = ddwdt.RestartGeneration
		d.structure = descriptor.AttachedStructure
	}
	if !d.initialized || d.structure == nil || extPkt.Packet.SSRC != d.ssrc {
		return false
	}

	extOutFrameNum := extFrameNum + d.frameNumOffset
	if extOutFrameNum > d.extLastFrameNum {
		d.extLastFrameNum = extOutFrameNum
	}

	d.descriptor = *descriptor
	d.descriptor.FrameNumber = uint16(extOutFrameNum)
	d.descriptor.ActiveDecodeTargetsBitmask = nil
	if descriptor.AttachedStructure == nil {
		// tell the subscriber which decode targets to expect, the dropped temporal layers
		// are not coming
		d.activeDecodeTargetsBitmask = 0
		for _, dt := range ddwdt.DecodeTargets {
			if dt.Layer.Temporal <= temporal {
				d.activeDecodeTargetsBitmask |= 1 << dt.Target
			}
		}
		d.descriptor.ActiveDecodeTargetsBitmask = &d.activeDecodeTargetsBitmask
	}

	if err := result.marshalDependencyDescriptorExtension(&dd.DependencyDescriptorExtension{
		Descriptor: &d.descriptor,
		Structure:  d.structure,
	}); err != nil {
		d.logger.Warnw("could not marshal dependency descriptor", err, "extFrameNum", extFrameNum)
		return false
	}
	return true
}
