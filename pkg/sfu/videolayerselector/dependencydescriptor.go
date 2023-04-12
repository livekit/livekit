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

	needsDecodeTargetBitmask   bool
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

func (d *DependencyDescriptor) Select(extPkt *buffer.ExtPacket, _layer int32) (result VideoLayerSelectorResult) {
	ddwdt := extPkt.DependencyDescriptor
	if ddwdt == nil {
		// packet doesn't have dependency descriptor
		return
	}

	dd := ddwdt.Descriptor

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
		// a packet of an alreadty forwarded frame, maintain decision
		result.RTPMarker = extPkt.Packet.Header.Marker || (dd.LastPacketInFrame && d.currentLayer.Spatial == int32(fd.SpatialId))
		result.IsSelected = true

	case selectorDecisionDropped:
		// a packet of an alreadty dropped frame, maintain decision
		return
	}

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
	if !isDecodable {
		// DD-TODO START
		// Not decodable could happen due to packet loss or out-of-order packets,
		// Need to figure out better ways to handle this.
		//
		// 1. Should definitely check if this frame is not part of current decode target OR discardable.
		//    In that case, forwarding can proceed without disruption.
		// 2. Add a packet queue and try to de-jitter for some time. Safest is to packet copy to local queue on
		//    all down tracks.
		// 3. Force a PLI and wait for a key frame.
		// DD-TODO END
		d.decisions.AddDropped(extFrameNum)
		return
	}

	// DD-TODO should not update for out-of-order RTP packets
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
	for _, dt := range ddwdt.DecodeTargets {
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

		chainIdx := d.structure.DecodeTargetProtectedByChain[dt.Target]
		if len(fd.ChainDiffs) < chainIdx {
			// look for lower target
			continue
		}

		prevFrameInChain := extFrameNum - uint64(fd.ChainDiffs[chainIdx])
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
		// no active decode target, do not select
		//d.logger.Debugw(fmt.Sprintf("drop packet for no target found, decodeTargets %v, tagetLayer %v, incoming %v",
		//d.decodeTargets,
		//d.targetLayer,
		//incomingLayer,
		//))
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
		d.decisions.AddDropped(extFrameNum)
		return
	}

	if d.currentLayer != highestDecodeTarget.Layer {
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
		Descriptor: dd,
		Structure:  d.structure,
	}
	if dd.AttachedStructure == nil {
		if d.needsDecodeTargetBitmask {
			d.needsDecodeTargetBitmask = false

			d.activeDecodeTargetsBitmask = buffer.GetActiveDecodeTargetBitmask(d.targetLayer, ddwdt.DecodeTargets)
			d.logger.Debugw("setting decode target bitmask", "activeDecodeTargetsBitmask", d.activeDecodeTargetsBitmask)
		}

		if d.activeDecodeTargetsBitmask != nil {
			// clone and override activebitmask
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

func (d *DependencyDescriptor) SetTarget(targetLayer buffer.VideoLayer) {
	if targetLayer == d.targetLayer {
		return
	}

	d.Base.SetTarget(targetLayer)

	d.needsDecodeTargetBitmask = true
}

func (d *DependencyDescriptor) updateDependencyStructure(structure *dede.FrameDependencyStructure) {
	d.structure = structure
}
