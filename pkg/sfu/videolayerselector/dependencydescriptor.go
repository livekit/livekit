package videolayerselector

import (
	"fmt"
	"sync"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	dede "github.com/livekit/livekit-server/pkg/sfu/dependencydescriptor"
	"github.com/livekit/livekit-server/pkg/sfu/utils"
	"github.com/livekit/protocol/logger"
)

type DependencyDescriptor struct {
	*Base

	frameNum  *utils.WrapAround[uint16, uint64]
	decisions *SelectorDecisionCache

	previousActiveDecodeTargetsBitmask *uint32
	activeDecodeTargetsBitmask         *uint32
	structure                          *dede.FrameDependencyStructure

	chains []*FrameChain

	decodeTargetsLock sync.RWMutex
	decodeTargets     []*DecodeTarget
}

func NewDependencyDescriptor(logger logger.Logger) *DependencyDescriptor {
	return &DependencyDescriptor{
		Base:      NewBase(logger),
		frameNum:  utils.NewWrapAround[uint16, uint64](),
		decisions: NewSelectorDecisionCache(256, 80),
	}
}

func NewDependencyDescriptorFromNull(vls VideoLayerSelector) *DependencyDescriptor {
	return &DependencyDescriptor{
		Base:      vls.(*Null).Base,
		frameNum:  utils.NewWrapAround[uint16, uint64](),
		decisions: NewSelectorDecisionCache(256, 80),
	}
}

func (d *DependencyDescriptor) IsOvershootOkay() bool {
	return false
}

func (d *DependencyDescriptor) Select(extPkt *buffer.ExtPacket, _layer int32) (result VideoLayerSelectorResult) {
	// a packet is always relevant for the svc codec
	result.IsRelevant = true

	ddwdt := extPkt.DependencyDescriptor
	if ddwdt == nil {
		// packet doesn't have dependency descriptor
		return
	}

	dd := ddwdt.Descriptor

	frameNum := d.frameNum.Update(dd.FrameNumber)
	extFrameNum := frameNum.ExtendedVal

	fd := dd.FrameDependencies
	incomingLayer := buffer.VideoLayer{
		Spatial:  int32(fd.SpatialId),
		Temporal: int32(fd.TemporalId),
	}

	// early return if this frame is already forwarded or dropped
	sd, err := d.decisions.GetDecision(extFrameNum)
	if err != nil {
		// do not mark as dropped as only error is an old frame
		return
	}
	switch sd {
	case selectorDecisionDropped:
		// a packet of an alreadty dropped frame, maintain decision
		return
	}

	if !d.currentLayer.IsValid() && !extPkt.KeyFrame {
		d.decisions.AddDropped(extFrameNum)
		return
	}

	if ddwdt.StructureUpdated {
		d.updateDependencyStructure(dd.AttachedStructure, ddwdt.DecodeTargets)
	}

	if ddwdt.ActiveDecodeTargetsUpdated {
		d.updateActiveDecodeTargets(*dd.ActiveDecodeTargetsBitmask)
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
	for _, dt := range d.decodeTargets {
		if !dt.Active() || dt.Layer.Spatial > d.targetLayer.Spatial || dt.Layer.Temporal > d.targetLayer.Temporal {
			continue
		}

		frameResult, err := dt.OnFrame(extFrameNum, fd)
		if err != nil {
			d.decodeTargetsLock.RUnlock()
			// dtis error, dependency descriptor might lost
			d.logger.Debugw(fmt.Sprintf("drop packet for frame detection error,  incoming: %v",
				incomingLayer,
			), "err", err)
			d.decisions.AddDropped(extFrameNum)
			return
		}

		// Keep forwarding the lower spatial with temporal layer 0 to keep the lower frame chain intact,
		// it will cost a few extra bits as those frames might not be present in the current target
		// but will make the subscriber switch to lower layer seamlessly without pli.
		if frameResult.TargetValid {
			if highestDecodeTarget.Target == -1 {
				highestDecodeTarget = dt.DependencyDescriptorDecodeTarget
				dti = frameResult.DTI
			} else if dt.Layer.Spatial < highestDecodeTarget.Layer.Spatial && dt.Layer.Temporal == 0 &&
				frameResult.DTI != dede.DecodeTargetNotPresent && frameResult.DTI != dede.DecodeTargetDiscardable {
				dti = frameResult.DTI
			}
		}
	}
	d.decodeTargetsLock.RUnlock()

	// DD-TODO : we don't have a rtp queue to ensure the order of packets now,
	// so we don't know packet is lost/out of order, that cause us can't detect
	// frame integrity, entire frame is forwareded, whether frame chain is broken.
	// So use a simple check here, assume all the reference frame is forwarded and
	// only check DTI of the active decode target.
	// it is not effeciency, at last we need check frame chain integrity.

	if highestDecodeTarget.Target < 0 {
		// no active decode target, do not select
		// d.logger.Debugw(fmt.Sprintf("drop packet for no target found, decodeTargets %v, tagetLayer %v, incoming %v",
		// 	d.decodeTargets,
		// 	d.targetLayer,
		// 	incomingLayer,
		// ))
		d.decisions.AddDropped(extFrameNum)
		return
	}

	// // DD-TODO : if bandwidth in congest, could drop the 'Discardable' frame
	if dti == dede.DecodeTargetNotPresent {
		// d.logger.Debugw(fmt.Sprintf("drop packet for decode target not present, highestDecodeTarget %d, incoming %v, fn: %d/%d",
		// 	highestDecodeTarget,
		// 	incomingLayer,
		// 	dd.FrameNumber,
		// 	extFrameNum,
		// ))
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
		d.decisions.AddDropped(extFrameNum)
		return
	}

	if d.currentLayer != highestDecodeTarget.Layer {
		result.IsSwitching = true
		if !d.currentLayer.IsValid() {
			result.IsResuming = true
			d.logger.Infow(
				"resuming at layer",
				"current", incomingLayer,
				"target", d.targetLayer,
				"max", d.maxLayer,
				"layer", fd.SpatialId,
				"req", d.requestSpatial,
				"maxSeen", d.maxSeenLayer,
				"feed", extPkt.Packet.SSRC,
			)
		}

		d.previousLayer = d.currentLayer
		d.currentLayer = highestDecodeTarget.Layer

		d.previousActiveDecodeTargetsBitmask = d.activeDecodeTargetsBitmask
		d.activeDecodeTargetsBitmask = buffer.GetActiveDecodeTargetBitmask(d.currentLayer, ddwdt.DecodeTargets)

		if d.currentLayer.Spatial == d.requestSpatial {
			result.IsSwitchingToRequestSpatial = true
		}
		if d.currentLayer.Spatial == d.maxLayer.Spatial {
			result.IsSwitchingToMaxSpatial = true
			result.MaxSpatialLayer = d.currentLayer.Spatial
			d.logger.Infow(
				"reached max layer",
				"previous", d.previousLayer,
				"current", d.currentLayer,
				"previousTarget", d.previousTargetLayer,
				"target", d.targetLayer,
				"max", d.maxLayer,
				"layer", fd.SpatialId,
				"req", d.requestSpatial,
				"maxSeen", d.maxSeenLayer,
				"feed", extPkt.Packet.SSRC,
			)
		}
	}

	ddExtension := &dede.DependencyDescriptorExtension{
		Descriptor: dd,
		Structure:  d.structure,
	}
	if dd.AttachedStructure == nil {
		if d.activeDecodeTargetsBitmask != nil {
			// clone and override activebitmask
			// DD-TODO: if the packet that contains the bitmask is acknowledged by RR, then we don't need it until it changed.
			ddClone := *ddExtension.Descriptor
			ddClone.ActiveDecodeTargetsBitmask = d.activeDecodeTargetsBitmask
			ddExtension.Descriptor = &ddClone
			// d.logger.Debugw("set active decode targets bitmask", "activeDecodeTargetsBitmask", d.activeDecodeTargetsBitmask)
		}
	}
	bytes, err := ddExtension.Marshal()
	if err != nil {
		d.logger.Warnw("error marshalling dependency descriptor extension", err)
	} else {
		result.DependencyDescriptorExtension = bytes
	}

	// DD-TODO START
	// Ideally should add this frame only on the last packet of the frame and if all packets of the frame have been selected.
	// But, adding on any packet so that any out-of-order packets within a frame can be fowarded.
	// But, that could result in decodability/chain integrity to erroneously pass (i. e. in the case of lost packet in this
	// frame, this frame is not decodable and hence the chain is broken).
	//
	// Note that packets can get lost in the forwarded path also. That will be handled by receiver sending PLI.
	//
	// Within SFU, there is more work to do to ensure integrity of forwarded packets/frames to adhere to the complete design
	// goal of dependency descriptor
	// DD-TODO END
	d.decisions.AddForwarded(extFrameNum)
	result.RTPMarker = extPkt.Packet.Header.Marker || (dd.LastPacketInFrame && d.currentLayer.Spatial == int32(fd.SpatialId))
	result.IsSelected = true
	return
}

func (d *DependencyDescriptor) Rollback() {
	d.activeDecodeTargetsBitmask = d.previousActiveDecodeTargetsBitmask

	d.Base.Rollback()
}

func (d *DependencyDescriptor) updateDependencyStructure(structure *dede.FrameDependencyStructure, decodeTargets []buffer.DependencyDescriptorDecodeTarget) {
	d.structure = structure

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

func (d *DependencyDescriptor) CheckSync() (locked bool, layer int32) {
	layer = d.GetRequestSpatial()
	if d.GetParked().IsValid() {
		return true, layer
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
