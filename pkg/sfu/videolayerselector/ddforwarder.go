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
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
)

// DDForwarder forwards the dependency descriptor of a simulcast stream. Without it, a
// subscriber of a stream without picture IDs, VP8 from libwebrtc for example, finds frame
// references by sequence number alone and stalls on every sequence number it cannot account
// for.
//
// libwebrtc numbers frames with one counter for all simulcast layers of a sender, so the
// frame numbers of a new layer are close to those of the old one. The numbers sent to the
// subscriber must still never go back: a key frame older than the last structure the
// subscriber received is dropped. So whenever the stream changes, a layer switch, a stream
// restart or a new forwarder, the numbers are moved up past the last one sent if needed.
// Otherwise they pass through with the current offset, so frame diffs and chain diffs stay
// valid. A migrated subscriber continues with the last number sent, which is part of the
// migrated forwarder state.
type DDForwarder struct {
	logger logger.Logger

	initialized       bool // a key frame with a structure set the stream
	ssrc              uint32
	restartGeneration int
	structure         *dd.FrameDependencyStructure

	frameNumOffset  uint64 // added to the incoming extended frame number
	hasLast         bool
	extLastFrameNum uint64 // highest outgoing extended frame number

	descriptor                 dd.DependencyDescriptor
	activeDecodeTargetsBitmask uint32
}

func NewDDForwarder(logger logger.Logger) *DDForwarder {
	return &DDForwarder{
		logger: logger,
	}
}

// NewDDForwarderFromOther continues the frame numbers of a forwarder it replaces on the
// same downtrack, a restarted upstream receiver for example.
func NewDDForwarderFromOther(other *DDForwarder, logger logger.Logger) *DDForwarder {
	d := NewDDForwarder(logger)
	if other != nil && other.hasLast {
		d.hasLast = true
		d.extLastFrameNum = other.extLastFrameNum
	}
	return d
}

// GetState returns the last frame number sent, nil before the first one.
func (d *DDForwarder) GetState() *livekit.DependencyDescriptorForwarderState {
	if !d.hasLast {
		return nil
	}
	return &livekit.DependencyDescriptorForwarderState{
		ExtLastFrameNumber: d.extLastFrameNum,
	}
}

// SeedState continues after the last frame number a forwarder on another node sent to the
// subscriber. Extended numbers there started from a lower or equal base, so the first key
// frame here moves past it, and the 16 bit numbers continue.
func (d *DDForwarder) SeedState(state *livekit.DependencyDescriptorForwarderState) {
	if state == nil {
		return
	}
	if !d.hasLast || state.ExtLastFrameNumber > d.extLastFrameNum {
		d.hasLast = true
		d.extLastFrameNum = state.ExtLastFrameNumber
	}
}

// Forward marshals the descriptor of a forwarded packet into result. temporal is the
// temporal layer the packet is forwarded at. It returns false when there is nothing to
// send, before the first key frame of the stream for example.
func (d *DDForwarder) Forward(extPkt *buffer.ExtPacket, temporal int32, result *VideoLayerSelectorResult) bool {
	ddwdt := extPkt.DependencyDescriptor
	if ddwdt == nil || ddwdt.Descriptor == nil || ddwdt.Descriptor.FrameDependencies == nil {
		return false
	}
	descriptor := ddwdt.Descriptor

	extFrameNum := ddwdt.ExtFrameNum
	// the parser accepts a structure only on the first packet of a frame
	hasStructure := descriptor.AttachedStructure != nil
	switch {
	case !d.initialized || extPkt.Packet.SSRC != d.ssrc:
		// a stream starts on a key frame, a late packet of the old layer is not described
		if !hasStructure {
			return false
		}
		d.rebase(extFrameNum)
		d.initialized = true
		d.ssrc = extPkt.Packet.SSRC
		d.restartGeneration = ddwdt.RestartGeneration

	case ddwdt.RestartGeneration != d.restartGeneration:
		// the restarted stream numbers its frames from a new start and has its own structure,
		// the first packet can be a delta frame
		d.rebase(extFrameNum)
		d.restartGeneration = ddwdt.RestartGeneration
		d.structure = nil
	}
	if hasStructure {
		d.structure = descriptor.AttachedStructure
	}
	if d.structure == nil {
		return false
	}

	extOutFrameNum := extFrameNum + d.frameNumOffset
	if !d.hasLast || extOutFrameNum > d.extLastFrameNum {
		d.hasLast = true
		d.extLastFrameNum = extOutFrameNum
	}

	// tell the subscriber which decode targets to expect: the publisher's active ones up to
	// the forwarded temporal layer. On a key frame too, where the structure alone would make
	// all of them active.
	d.activeDecodeTargetsBitmask = 0
	for _, dt := range ddwdt.DecodeTargets {
		if dt.Layer.Temporal <= temporal {
			d.activeDecodeTargetsBitmask |= 1 << dt.Target
		}
	}
	d.activeDecodeTargetsBitmask &= ddwdt.ActiveDecodeTargetsMask
	d.descriptor = *descriptor
	d.descriptor.FrameNumber = uint16(extOutFrameNum)
	d.descriptor.ActiveDecodeTargetsBitmask = &d.activeDecodeTargetsBitmask

	if err := result.marshalDependencyDescriptorExtension(&dd.DependencyDescriptorExtension{
		Descriptor: &d.descriptor,
		Structure:  d.structure,
	}); err != nil {
		d.logger.Warnw("could not marshal dependency descriptor", err, "extFrameNum", extFrameNum)
		return false
	}
	return true
}

// rebase moves the frame numbers up when a new stream would go back past the last one sent
func (d *DDForwarder) rebase(extFrameNum uint64) {
	if d.hasLast && extFrameNum+d.frameNumOffset <= d.extLastFrameNum {
		d.frameNumOffset = d.extLastFrameNum + 1 - extFrameNum
	}
}
