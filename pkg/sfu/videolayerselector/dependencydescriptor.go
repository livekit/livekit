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
	"fmt"
	"runtime/debug"
	"sync"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	dede "github.com/livekit/livekit-server/pkg/sfu/rtpextension/dependencydescriptor"
	"github.com/livekit/protocol/logger"
)

const (
	decisionCacheMaxElements = 256
	decisionCacheNackEntries = 80
)

type DependencyDescriptor struct {
	*Base

	decisions *SelectorDecisionCache

	previousActiveDecodeTargetsBitmask *uint32
	activeDecodeTargetsBitmask         *uint32
	structure                          *dede.FrameDependencyStructure
	extKeyFrameNum                     uint64
	keyFrameValid                      bool

	chains []*FrameChain

	decodeTargetsLock sync.RWMutex
	decodeTargets     []*DecodeTarget
	fnWrapper         FrameNumberWrapper

	restartGeneration int
}

func NewDependencyDescriptor(logger logger.Logger) *DependencyDescriptor {
	return &DependencyDescriptor{
		Base:      NewBase(logger),
		decisions: NewSelectorDecisionCache(decisionCacheMaxElements, decisionCacheNackEntries),
		fnWrapper: FrameNumberWrapper{logger: logger},
	}
}

func NewDependencyDescriptorFromOther(vls VideoLayerSelector) *DependencyDescriptor {
	return &DependencyDescriptor{
		Base:      vls.getBase(),
		decisions: NewSelectorDecisionCache(256, 80),
		fnWrapper: FrameNumberWrapper{logger: vls.getLogger()},
	}
}

func (d *DependencyDescriptor) IsOvershootOkay() bool {
	return false
}

func (d *DependencyDescriptor) Select(extPkt *buffer.ExtPacket, _layer int32) (result VideoLayerSelectorResult) {
	// a packet is always relevant for the svc codec
	if d.currentLayer.IsValid() {
		result.IsRelevant = true
	}

	ddwdt := extPkt.DependencyDescriptor
	if ddwdt == nil {
		// packet doesn't have dependency descriptor
		// d.logger.Debugw(fmt.Sprintf("drop packet, no DD, incoming %v, sn: %d, isKeyFrame: %v", extPkt.VideoLayer, extPkt.Packet.SequenceNumber, extPkt.KeyFrame))
		return
	}

	if ddwdt.RestartGeneration > d.restartGeneration {
		d.logger.Debugw("stream restarted",
			"packet", ddwdt.RestartGeneration,
			"current", d.restartGeneration,
			"structureKeyFrame", d.extKeyFrameNum,
			"efn", ddwdt.ExtFrameNum,
			"lastEfn", d.fnWrapper.LastOrigin(),
		)
		d.restart(ddwdt.RestartGeneration)
	} else if ddwdt.RestartGeneration < d.restartGeneration {
		// must not happen
		d.logger.Warnw("packet from old generation", nil, "packet", ddwdt.RestartGeneration, "current", d.restartGeneration)
	}

	dd := ddwdt.Descriptor

	extFrameNum := ddwdt.ExtFrameNum

	fd := dd.FrameDependencies
	incomingLayer := buffer.VideoLayer{
		Spatial:  int32(fd.SpatialId),
		Temporal: int32(fd.TemporalId),
	}

	if !d.keyFrameValid && dd.AttachedStructure == nil {
		// d.logger.Debugw(fmt.Sprintf("drop packet, no attached structure, incoming %v, sn: %d, isKeyFrame: %v", extPkt.VideoLayer, extPkt.Packet.SequenceNumber, extPkt.KeyFrame))
		return
	}

	// early return if this frame is already forwarded or dropped
	sd, err := d.decisions.GetDecision(extFrameNum)
	if err != nil {
		// do not mark as dropped as only error is an old frame
		// d.logger.Debugw(fmt.Sprintf("drop packet on decision error, incoming %v, fn: %d/%d, sn: %d",
		//	incomingLayer,
		//	dd.FrameNumber,
		//	extFrameNum,
		//	extPkt.Packet.SequenceNumber,
		// ), "err", err)
		return
	}
	switch sd {
	case selectorDecisionDropped:
		// a packet of an alreadty dropped frame, maintain decision
		// d.logger.Debugw(fmt.Sprintf("drop packet already dropped, incoming %v, fn: %d/%d, sn: %d",
		//	incomingLayer,
		//	dd.FrameNumber,
		//	extFrameNum,
		//	extPkt.Packet.SequenceNumber,
		// ))
		return
	}

	if ddwdt.StructureUpdated {
		// d.logger.Debugw("update dependency structure",
		// 	"structureID", dd.AttachedStructure.StructureId,
		// 	"structure", dd.AttachedStructure,
		// 	"decodeTargets", ddwdt.DecodeTargets,
		// 	"efn", extFrameNum,
		// 	"sn", extPkt.Packet.SequenceNumber,
		// 	"isKeyFrame", extPkt.KeyFrame,
		// 	"currentKeyframe", d.extKeyFrameNum,
		// )

		d.updateDependencyStructure(dd.AttachedStructure, ddwdt.DecodeTargets, extFrameNum)
	}

	if ddwdt.ExtKeyFrameNum != d.extKeyFrameNum {
		// keyframe mismatch, drop and reset chains
		d.logger.Debugw("drop packet for keyframe mismatch", "incoming", incomingLayer, "efn", extFrameNum, "sn", extPkt.Packet.SequenceNumber, "requiredKeyFrame", ddwdt.ExtKeyFrameNum, "structureKeyFrame", d.extKeyFrameNum)
		d.decisions.AddDropped(extFrameNum)
		d.invalidateKeyFrame()
		return
	}

	if ddwdt.ActiveDecodeTargetsUpdated {
		d.updateActiveDecodeTargets(*dd.ActiveDecodeTargetsBitmask)
	}

	if len(fd.ChainDiffs) != len(d.chains) {
		d.logger.Debugw("frame chain diff length mismatch", nil,
			"incoming", incomingLayer,
			"efn", extFrameNum,
			"sn", extPkt.Packet.SequenceNumber,
			"chainDiffs", fd.ChainDiffs,
			"chains", len(d.chains),
			"requiredKeyFrame", ddwdt.ExtKeyFrameNum,
			"structureKeyFrame", d.extKeyFrameNum,
		)
		d.decisions.AddDropped(extFrameNum)
		return
	}

	for _, chain := range d.chains {
		chain.OnFrame(extFrameNum, fd)
	}

	// find decode target closest to targetLayer
	highestDecodeTarget := buffer.DependencyDescriptorDecodeTarget{
		Target: -1,
		Layer:  buffer.InvalidLayer,
	}
	var dti dede.DecodeTargetIndication
	d.decodeTargetsLock.RLock()

	// decodeTargets be sorted from high to low, find the highest decode target that is active and integrity
	for _, dt := range d.decodeTargets {
		if !dt.Active() || dt.Layer.Spatial > d.targetLayer.Spatial || dt.Layer.Temporal > d.targetLayer.Temporal {
			continue
		}

		frameResult, err := dt.OnFrame(extFrameNum, fd)
		if err != nil {
			d.decodeTargetsLock.RUnlock()
			// dtis error, dependency descriptor might lost
			d.logger.Warnw(fmt.Sprintf("drop packet for frame detection error,  incoming: %v", incomingLayer), err)
			d.decisions.AddDropped(extFrameNum)
			return
		}

		if frameResult.TargetValid {
			highestDecodeTarget = dt.DependencyDescriptorDecodeTarget
			dti = frameResult.DTI
			break
		}
	}
	d.decodeTargetsLock.RUnlock()

	if highestDecodeTarget.Target < 0 {
		// no active decode target, do not select
		// d.logger.Debugw(
		//	"drop packet for no target found",
		//	"highestDecodeTarget", highestDecodeTarget,
		//	"decodeTargets", d.decodeTargets,
		//	"tagetLayer", d.targetLayer,
		//	"incoming", incomingLayer,
		//	"fn", dd.FrameNumber,
		//	"efn", extFrameNum,
		//	"sn", extPkt.Packet.SequenceNumber,
		//	"isKeyFrame", extPkt.KeyFrame,
		// )
		d.decisions.AddDropped(extFrameNum)
		return
	}

	// DD-TODO : if bandwidth in congest, could drop the 'Discardable' frame
	if dti == dede.DecodeTargetNotPresent {
		// d.logger.Debugw(
		//	"drop packet for decode target not present",
		//	"highestDecodeTarget", highestDecodeTarget,
		//	"decodeTargets", d.decodeTargets,
		//	"tagetLayer", d.targetLayer,
		//	"incoming", incomingLayer,
		//	"fn", dd.FrameNumber,
		//	"efn", extFrameNum,
		//	"sn", extPkt.Packet.SequenceNumber,
		//	"isKeyFrame", extPkt.KeyFrame,
		// )
		d.decisions.AddDropped(extFrameNum)
		return
	}

	// check decodability using reference frames
	isDecodable := true
	for _, fdiff := range fd.FrameDiffs {
		if fdiff == 0 {
			continue
		}

		// use relaxed check for frame diff that we have chain intact detection and don't want
		// to drop packet due to out-of-order packet or recoverable packet loss
		if sd, _ := d.decisions.GetDecision(extFrameNum - uint64(fdiff)); sd == selectorDecisionDropped {
			isDecodable = false
			break
		}
	}
	if !isDecodable {
		// d.logger.Debugw(
		//	"drop packet for not decodable",
		//	"highestDecodeTarget", highestDecodeTarget,
		//	"decodeTargets", d.decodeTargets,
		//	"tagetLayer", d.targetLayer,
		//	"incoming", incomingLayer,
		//	"fn", dd.FrameNumber,
		//	"efn", extFrameNum,
		//	"sn", extPkt.Packet.SequenceNumber,
		//	"isKeyFrame", extPkt.KeyFrame,
		// )
		d.decisions.AddDropped(extFrameNum)
		return
	}

	if d.currentLayer != highestDecodeTarget.Layer {
		result.IsSwitching = true
		if !d.currentLayer.IsValid() {
			result.IsResuming = true
			d.logger.Debugw(
				"resuming at layer",
				"current", incomingLayer,
				"target", d.targetLayer,
				"max", d.maxLayer,
				"layer", fd.SpatialId,
				"req", d.requestSpatial,
				"maxSeen", d.maxSeenLayer,
				"feed", extPkt.Packet.SSRC,
				"fn", dd.FrameNumber,
				"efn", extFrameNum,
				"sn", extPkt.Packet.SequenceNumber,
				"isKeyFrame", extPkt.KeyFrame,
			)
		}

		d.previousLayer = d.currentLayer
		d.currentLayer = highestDecodeTarget.Layer

		d.previousActiveDecodeTargetsBitmask = d.activeDecodeTargetsBitmask
		d.activeDecodeTargetsBitmask = buffer.GetActiveDecodeTargetBitmask(d.currentLayer, ddwdt.DecodeTargets)
		d.logger.Debugw(
			"switch to target",
			"highestDecodeTarget", highestDecodeTarget,
			"previous", d.previousLayer,
			"bitmask", *d.activeDecodeTargetsBitmask,
			"fn", dd.FrameNumber,
			"efn", extFrameNum,
			"sn", extPkt.Packet.SequenceNumber,
			"isKeyFrame", extPkt.KeyFrame,
		)

		result.IsRelevant = true
	}

	ddExtension := &dede.DependencyDescriptorExtension{
		Descriptor: dd,
		Structure:  d.structure,
	}

	unWrapFn := uint16(d.fnWrapper.UpdateAndGet(extFrameNum, ddwdt.StructureUpdated))
	var ddClone *dede.DependencyDescriptor
	if unWrapFn != dd.FrameNumber {
		clone := *dd
		ddClone = &clone
		ddClone.FrameNumber = unWrapFn
		ddExtension.Descriptor = ddClone
	}

	if dd.AttachedStructure == nil {
		if d.activeDecodeTargetsBitmask != nil {
			if ddClone == nil {
				// clone and override activebitmask
				// DD-TODO: if the packet that contains the bitmask is acknowledged by RR, then we don't need it until it changed.
				clone := *dd
				ddClone = &clone
				ddExtension.Descriptor = ddClone
			}
			ddClone.ActiveDecodeTargetsBitmask = d.activeDecodeTargetsBitmask
			// d.logger.Debugw("set active decode targets bitmask", "activeDecodeTargetsBitmask", d.activeDecodeTargetsBitmask)
		}
	}

	var ddMarshaled bool
	func() {
		defer func() {
			if r := recover(); r != nil {
				d.logger.Errorw("panic marshalling dependency descriptor extension", nil,
					"efn", extFrameNum,
					"sn", extPkt.Packet.SequenceNumber,
					"keyframeRequired", ddwdt.ExtKeyFrameNum,
					"currentKeyframe", d.extKeyFrameNum,
					"panic", r,
					"stack", string(debug.Stack()))
			}
		}()
		bytes, err := ddExtension.Marshal()
		if err != nil {
			d.logger.Warnw("error marshalling dependency descriptor extension", err)
		} else {
			result.DependencyDescriptorExtension = bytes
			ddMarshaled = true
		}
	}()

	if !ddMarshaled {
		// drop packet if we can't marshal dependency descriptor
		d.decisions.AddDropped(extFrameNum)
		return
	}

	if ddwdt.Integrity {
		d.decisions.AddForwarded(extFrameNum)
	}
	result.RTPMarker = extPkt.Packet.Header.Marker || (dd.LastPacketInFrame && d.currentLayer.Spatial == int32(fd.SpatialId))
	result.IsSelected = true
	return
}

func (d *DependencyDescriptor) Rollback() {
	d.activeDecodeTargetsBitmask = d.previousActiveDecodeTargetsBitmask

	d.Base.Rollback()
}

func (d *DependencyDescriptor) updateDependencyStructure(structure *dede.FrameDependencyStructure, decodeTargets []buffer.DependencyDescriptorDecodeTarget, extFrameNum uint64) {
	d.structure = structure
	d.extKeyFrameNum = extFrameNum
	d.keyFrameValid = true

	d.chains = d.chains[:0]

	for chainIdx := 0; chainIdx < structure.NumChains; chainIdx++ {
		d.chains = append(d.chains, NewFrameChain(d.decisions, chainIdx, d.logger))
	}

	newTargets := make([]*DecodeTarget, 0, len(decodeTargets))
	for _, dt := range decodeTargets {
		var chain *FrameChain
		// When chain_cnt > 0, each Decode target MUST be protected by exactly one Chain.
		if structure.NumChains > 0 {
			chainIdx := structure.DecodeTargetProtectedByChain[dt.Target]
			if chainIdx >= len(d.chains) {
				// should not happen
				d.logger.Errorw("DecodeTargetProtectedByChain chainIdx out of range", nil, "chainIdx", chainIdx, "NumChains", len(d.chains))
			} else {
				chain = d.chains[chainIdx]
			}
		}
		newTargets = append(newTargets, NewDecodeTarget(dt, chain))
	}
	d.decodeTargetsLock.Lock()
	d.decodeTargets = newTargets
	d.decodeTargetsLock.Unlock()
}

func (d *DependencyDescriptor) updateActiveDecodeTargets(activeDecodeTargetsBitmask uint32) {
	for _, chain := range d.chains {
		chain.BeginUpdateActive()
	}

	d.decodeTargetsLock.RLock()
	for _, dt := range d.decodeTargets {
		dt.UpdateActive(activeDecodeTargetsBitmask)
	}
	d.decodeTargetsLock.RUnlock()

	for _, chain := range d.chains {
		chain.EndUpdateActive()
	}
}

func (d *DependencyDescriptor) invalidateKeyFrame() {
	d.keyFrameValid = false
	d.chains = d.chains[:0]
	d.decodeTargetsLock.Lock()
	d.decodeTargets = d.decodeTargets[:0]
	d.decodeTargetsLock.Unlock()
}

func (d *DependencyDescriptor) CheckSync() (locked bool, layer int32) {
	layer = d.GetRequestSpatial()
	if !d.currentLayer.IsValid() || !d.keyFrameValid {
		// always declare not locked when trying to resume from nothing
		return false, layer
	}

	d.decodeTargetsLock.RLock()
	defer d.decodeTargetsLock.RUnlock()
	for _, dt := range d.decodeTargets {
		if dt.Active() && dt.Layer.Spatial == layer && dt.Valid() {
			return true, layer
		}
	}

	return false, layer
}

func (d *DependencyDescriptor) restart(generation int) {
	d.restartGeneration = generation
	d.invalidateKeyFrame()
	d.decisions = NewSelectorDecisionCache(decisionCacheMaxElements, decisionCacheNackEntries)
}
