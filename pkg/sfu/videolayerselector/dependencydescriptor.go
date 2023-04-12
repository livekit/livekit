package videolayerselector

import (
	"fmt"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	dede "github.com/livekit/livekit-server/pkg/sfu/dependencydescriptor"
	"github.com/livekit/livekit-server/pkg/sfu/utils"
	"github.com/livekit/protocol/logger"
)

type DependencyDescriptor struct {
	*Base

	frameNum  *utils.WrapAround[uint16, uint64]
	decisions *SelectorDecisionCache

	decodeTargets              []buffer.DependencyDescriptorDecodeTarget // RAJA-TODO: get this from extPkt
	activeDecodeTargetsBitmask *uint32
	structure                  *dede.FrameDependencyStructure
}

func NewDependencyDescriptor(logger logger.Logger) *DependencyDescriptor {
	return &DependencyDescriptor{
		Base:      NewBase(logger),
		frameNum:  utils.NewWrapAround[uint16, uint64](),
		decisions: NewSelectorDecisionCache(256),
	}
}

func NewDependencyDescriptorFromNull(vls VideoLayerSelector) *DependencyDescriptor {
	return &DependencyDescriptor{
		Base:      vls.(*Null).Base,
		frameNum:  utils.NewWrapAround[uint16, uint64](),
		decisions: NewSelectorDecisionCache(256),
	}
}

func (d *DependencyDescriptor) IsOvershootOkay() bool {
	return false
}

// RAJA-TODO
// - check frame diffs and decode chains
// - frame number wrapper
// - bit set to store forwarded frames
//   o set on any packet of forwarded frame
//   o when frame is forwarded, forward other packets without other checks
//   o (maybe) use two bits to indicate missing, forwarded, dropped
// - on a switch point, set current to decode target (both spatial and temporal)
// - set RTP marker on current spatial match and end-of-frame
// - need some notion of chain broken and wait for key frame
// Questions
// - what happens on packet loss?
// - what happens on out-of-order?

func (d *DependencyDescriptor) Select(extPkt *buffer.ExtPacket, _layer int32) (result VideoLayerSelectorResult) {
	dd := extPkt.DependencyDescriptor
	if dd == nil {
		// packet doesn't have dependency descriptor
		return
	}

	// a packet is relevant as long as it has DD extension
	result.IsRelevant = true

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
	case selectorDecisionForwarded:
		result.RTPMarker = extPkt.Packet.Header.Marker || (dd.LastPacketInFrame && d.currentLayer.Spatial == int32(fd.SpatialId))
		d.logger.Infow("RAJA forwarding2", "info", fmt.Sprintf("fn: %d/%d, incoming: %+v, current: %+v, marker: %+v", dd.FrameNumber, extFrameNum, incomingLayer, d.currentLayer, result.RTPMarker)) // REMOVE
		result.IsSelected = true

	case selectorDecisionDropped:
		d.logger.Infow("RAJA dropping2", "info", fmt.Sprintf("fn: %d/%d, incoming: %+v, current: %+v, marker: %+v", dd.FrameNumber, extFrameNum, incomingLayer, d.currentLayer, result.RTPMarker)) // REMOVE
		return
	}

	// RAJA-TODO - maybe add a struct field `waitingForKeyFrame` and add to check here
	// RAJA-TODO - maybe this is not required
	if !d.currentLayer.IsValid() && !extPkt.KeyFrame {
		d.decisions.AddDropped(extFrameNum)
		return
	}

	// check decodability using reference frames
	isDecodable := true
	for _, fdiff := range fd.FrameDiffs {
		if fdiff == 0 {
			continue
		}

		if sd, _ := d.decisions.GetDecision(extFrameNum - uint64(fdiff)); sd != selectorDecisionForwarded {
			isDecodable = false
			break
		}
	}
	// RAJA-TODO: check if not decodable, can we drop this early?
	if !isDecodable {
		d.decisions.AddDropped(extFrameNum)
		return
	}

	// RAJA-TODO should not update for out-of-order RTP packets
	if dd.AttachedStructure != nil {
		// update decode target layer and active decode targets
		// DD-TODO : these targets info can be shared by all the downtracks, no need calculate in every selector
		d.updateDependencyStructure(dd.AttachedStructure)
	}

	// DD-TODO : we don't have a rtp queue to ensure the order of packets now,
	// so we don't know packet is lost/out of order, that cause us can't detect
	// frame integrity, entire frame is forwareded, whether frame chain is broken.
	// So use a simple check here, assume all the reference frame is forwarded and
	// only check DTI of the active decode target.
	// it is not effeciency, at last we need check frame chain integrity.

	activeDecodeTargets := dd.ActiveDecodeTargetsBitmask
	if activeDecodeTargets != nil {
		d.logger.Debugw("active decode targets", "activeDecodeTargets", *activeDecodeTargets)
	}

	// find decode target closest to targetLayer
	highestDecodeTarget := buffer.DependencyDescriptorDecodeTarget{
		Target: -1,
		Layer:  buffer.InvalidLayer,
	}
	for _, dt := range d.decodeTargets {
		if dt.Layer.Spatial > d.targetLayer.Spatial || dt.Layer.Temporal > d.targetLayer.Temporal {
			continue
		}

		if activeDecodeTargets != nil && ((*activeDecodeTargets)&(1<<dt.Target) == 0) {
			continue
		}

		if len(d.structure.DecodeTargetProtectedByChain) == 0 {
			highestDecodeTarget = dt
			//d.logger.Debugw("select target", "highestDecodeTarget", highestDecodeTarget, "dtis", fd.DecodeTargetIndications)
			break
		}

		if len(d.structure.DecodeTargetProtectedByChain) < dt.Target {
			// look for lower target
			continue
		}

		chainID := d.structure.DecodeTargetProtectedByChain[dt.Target]
		if len(fd.ChainDiffs) < chainID {
			// look for lower target
			continue
		}

		prevFrameInChain := extFrameNum - uint64(fd.ChainDiffs[chainID])
		if prevFrameInChain != 0 && prevFrameInChain != extFrameNum {
			if sd, err := d.decisions.GetDecision(prevFrameInChain); err != nil || sd != selectorDecisionForwarded {
				// look for lower target
				continue
			}
		}

		highestDecodeTarget = dt
		//d.logger.Debugw("select target", "highestDecodeTarget", highestDecodeTarget, "dtis", fd.DecodeTargetIndications)
		break
	}

	if highestDecodeTarget.Target < 0 {
		//d.logger.Debugw(fmt.Sprintf("drop packet for no target found, decodeTargets %v, tagetLayer %v, incoming %v",
		//d.decodeTargets,
		//d.targetLayer,
		//incomingLayer,
		//))
		d.logger.Debugw(fmt.Sprintf("RAJA drop packet for no target found, decodeTargets %v, tagetLayer %v, incoming %v, structure: %+v, fn: %d/%d, fd: %+v, dec: %+v",
			d.decodeTargets,
			d.targetLayer,
			incomingLayer,
			d.structure,
			dd.FrameNumber,
			extFrameNum,
			fd,
			isDecodable,
		)) // REMOVE

		// no active decode target, do not select
		d.decisions.AddDropped(extFrameNum)
		return
	}

	dtis := fd.DecodeTargetIndications
	if len(dtis) < highestDecodeTarget.Target {
		// dtis error, dependency descriptor might lost
		d.logger.Debugw(fmt.Sprintf("drop packet for dtis error, dtis %v, highestDecodeTarget %+v, incoming: %v",
			dtis,
			highestDecodeTarget,
			incomingLayer,
		))
		d.logger.Debugw(fmt.Sprintf("RAJA drop packet for dtis error, dtis %v, highestTarget %+v, incoming %v, dec: %+v",
			dtis,
			highestDecodeTarget,
			incomingLayer,
			isDecodable,
		)) // REMOVE
		d.decisions.AddDropped(extFrameNum)
		return
	}

	// DD-TODO : if bandwidth in congest, could drop the 'Discardable' packet
	dti := dtis[highestDecodeTarget.Target]
	if dti == dede.DecodeTargetNotPresent {
		//d.logger.Debugw(fmt.Sprintf("drop packet for decode target not present, dtis %v, highestDecodeTarget %d, incoming %v, fn: %d/%d",
		//dtis,
		//highestDecodeTarget,
		//incomingLayer,
		//dd.FrameNumber,
		//extFrameNum,
		//))
		d.logger.Debugw(fmt.Sprintf("RAJA drop packet for decode target not present, dtis %v, highestDecodeTarget %d, incoming %v, fn: %d/%d, dec: %+v",
			dtis,
			highestDecodeTarget,
			incomingLayer,
			dd.FrameNumber,
			extFrameNum,
			isDecodable,
		)) // REMOVE
		d.decisions.AddDropped(extFrameNum)
		return
	}

	if dti == dede.DecodeTargetSwitch && d.currentLayer != highestDecodeTarget.Layer {
		d.logger.Infow("RAJA switching", "current", d.currentLayer, "incoming", incomingLayer, "target", d.targetLayer, "highestDecodeTarget", highestDecodeTarget) // REMOVE
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
		d.currentLayer = highestDecodeTarget.Layer
		if d.currentLayer.Spatial == d.requestSpatial {
			result.IsSwitchingToRequestSpatial = true
		}
		if d.currentLayer.Spatial == d.maxLayer.Spatial {
			result.IsSwitchingToMaxSpatial = true
			d.logger.Infow(
				"reached max layer",
				"current", d.currentLayer,
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
		Descriptor: extPkt.DependencyDescriptor,
		Structure:  d.structure,
	}
	if extPkt.DependencyDescriptor.AttachedStructure == nil && d.activeDecodeTargetsBitmask != nil {
		// clone and override activebitmask
		ddClone := *ddExtension.Descriptor
		ddClone.ActiveDecodeTargetsBitmask = d.activeDecodeTargetsBitmask
		ddExtension.Descriptor = &ddClone
		// d.logger.Debugw("set active decode targets bitmask", "activeDecodeTargetsBitmask", d.activeDecodeTargetsBitmask)
	}
	bytes, err := ddExtension.Marshal()
	if err != nil {
		d.logger.Warnw("error marshalling dependency descriptor extension", err)
	} else {
		result.DependencyDescriptorExtension = bytes
	}

	d.decisions.AddForwarded(extFrameNum)
	result.RTPMarker = extPkt.Packet.Header.Marker || (dd.LastPacketInFrame && d.currentLayer.Spatial == int32(fd.SpatialId))
	d.logger.Infow("RAJA forwarding", "info", fmt.Sprintf("fn: %d/%d, incoming: %+v, current: %+v, marker: %+v, isDecodable: %+v", dd.FrameNumber, extFrameNum, incomingLayer, d.currentLayer, result.RTPMarker, isDecodable)) // REMOVE
	result.IsSelected = true
	return
}

func (d *DependencyDescriptor) SetTarget(targetLayer buffer.VideoLayer) {
	d.Base.SetTarget(targetLayer)

	/* RAJA-REMOVE
	activeBitMask := uint32(0)
	var maxSpatial, maxTemporal int32
	for _, dt := range d.decodeTargets {
		if dt.Layer.Spatial > maxSpatial {
			maxSpatial = dt.Layer.Spatial
		}
		if dt.Layer.Temporal > maxTemporal {
			maxTemporal = dt.Layer.Temporal
		}
		if dt.Layer.Spatial <= targetLayer.Spatial && dt.Layer.Temporal <= targetLayer.Temporal {
			activeBitMask |= 1 << dt.Target
		}
	}
	if targetLayer.Spatial == maxSpatial && targetLayer.Temporal == maxTemporal {
		// all the decode targets are selected
		d.activeDecodeTargetsBitmask = nil
	} else {
		d.activeDecodeTargetsBitmask = &activeBitMask
	}
	*/
	d.activeDecodeTargetsBitmask = buffer.GetActiveDecodeTargetBitmask(targetLayer, d.decodeTargets)
	d.logger.Debugw("setting target", "targetlayer", targetLayer, "activeDecodeTargetsBitmask", d.activeDecodeTargetsBitmask)
}

func (d *DependencyDescriptor) updateDependencyStructure(structure *dede.FrameDependencyStructure) {
	d.structure = structure
	/* RAJA-REMOVE
	d.decodeTargets = d.decodeTargets[:0]

	for target := 0; target < structure.NumDecodeTargets; target++ {
		layer := buffer.VideoLayer{Spatial: 0, Temporal: 0}
		for _, t := range structure.Templates {
			if t.DecodeTargetIndications[target] != dd.DecodeTargetNotPresent {
				if layer.Spatial < int32(t.SpatialId) {
					layer.Spatial = int32(t.SpatialId)
				}
				if layer.Temporal < int32(t.TemporalId) {
					layer.Temporal = int32(t.TemporalId)
				}
			}
		}
		d.decodeTargets = append(d.decodeTargets, decodeTarget{target, layer})
	}

	// sort decode target layer by spatial and temporal from high to low
	sort.Slice(d.decodeTargets, func(i, j int) bool {
		return d.decodeTargets[i].Layer.GreaterThan(d.decodeTargets[j].Layer)
	})
	*/
	d.decodeTargets = buffer.ProcessFrameDependencyStructure(structure)
	d.logger.Debugw(fmt.Sprintf("update decode targets: %v", d.decodeTargets))
}
