package videolayerselector

import (
	"fmt"
	"sort"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	dd "github.com/livekit/livekit-server/pkg/sfu/dependencydescriptor"
	"github.com/livekit/protocol/logger"
)

type targetLayer struct {
	Target int
	Layer  buffer.VideoLayer
}

type DependencyDescriptor struct {
	*Base

	// DD-TODO : fields for frame chain detect
	// frameNumberWrapper Uint16Wrapper
	// expectKeyFrame      bool

	decodeTargetLayer          []targetLayer
	activeDecodeTargetsBitmask *uint32
	structure                  *dd.FrameDependencyStructure
}

func NewDependencyDescriptor(logger logger.Logger) *DependencyDescriptor {
	return &DependencyDescriptor{
		Base: NewBase(logger),
	}
}

func NewDependencyDescriptorFromNull(vls VideoLayerSelector) *DependencyDescriptor {
	return &DependencyDescriptor{
		Base: vls.(*Null).Base,
	}
}

// RAJA-TODO: fill all results field properly
func (d *DependencyDescriptor) Select(extPkt *buffer.ExtPacket, _layer int32) (result VideoLayerSelectorResult) {
	result.IsRelevant = true
	result.RTPMarker = extPkt.Packet.Marker
	if extPkt.DependencyDescriptor == nil {
		// packet don't have dependency descriptor, pass check
		result.IsSelected = true
		return
	}

	if extPkt.DependencyDescriptor.AttachedStructure != nil {
		// update decode target layer and active decode targets
		// DD-TODO : these targets info can be shared by all the downtracks, no need calculate in every selector
		d.updateDependencyStructure(extPkt.DependencyDescriptor.AttachedStructure)
	}

	// DD-TODO : we don't have a rtp queue to ensure the order of packets now,
	// so we don't know packet is lost/out of order, that cause us can't detect
	// frame integrity, entire frame is forwareded, whether frame chain is broken.
	// So use a simple check here, assume all the reference frame is forwarded and
	// only check DTI of the active decode target.
	// it is not effeciency, at last we need check frame chain integrity.

	activeDecodeTargets := extPkt.DependencyDescriptor.ActiveDecodeTargetsBitmask
	if activeDecodeTargets != nil {
		d.logger.Debugw("active decode targets", "activeDecodeTargets", *activeDecodeTargets)
	}

	currentTarget := -1
	for _, dt := range d.decodeTargetLayer {
		// find target match with selected layer
		if dt.Layer.Spatial <= d.targetLayer.Spatial && dt.Layer.Temporal <= d.targetLayer.Temporal {
			if activeDecodeTargets == nil || ((*activeDecodeTargets)&(1<<dt.Target) != 0) {
				// DD-TODO : check frame chain integrity
				currentTarget = dt.Target
				// d.logger.Debugw("select target", "target", currentTarget, "layer", dt.layer, "dtis", extPkt.DependencyDescriptor.FrameDependencies.DecodeTargetIndications)
				break
			}
		}
	}

	if currentTarget < 0 {
		// d.logger.Debugw(fmt.Sprintf("drop packet for no target found, decodeTargets %v, selected layer %v, s:%d, t:%d",
		// s.decodeTargetLayer, s.layer, extPkt.DependencyDescriptor.FrameDependencies.SpatialId, extPkt.DependencyDescriptor.FrameDependencies.TemporalId))
		// no active decode target, forward all packets
		return
	}

	dtis := extPkt.DependencyDescriptor.FrameDependencies.DecodeTargetIndications
	if len(dtis) < currentTarget {
		// dtis error, dependency descriptor might lost
		d.logger.Debugw(fmt.Sprintf("drop packet for dtis error, dtis %v, currentTarget %d, s:%d, t:%d",
			dtis,
			currentTarget,
			extPkt.DependencyDescriptor.FrameDependencies.SpatialId,
			extPkt.DependencyDescriptor.FrameDependencies.TemporalId,
		))
		return
	}

	// DD-TODO : if bandwidth in congest, could drop the 'Discardable' packet
	if dti := dtis[currentTarget]; dti == dd.DecodeTargetNotPresent {
		// d.logger.Debugw(fmt.Sprintf("drop packet for decode target not present, dtis %v, currentTarget %d, s:%d, t:%d", dtis, currentTarget,
		// extPkt.DependencyDescriptor.FrameDependencies.SpatialId, extPkt.DependencyDescriptor.FrameDependencies.TemporalId))
		return
	} else if dti == dd.DecodeTargetSwitch {
		result.IsSwitchingLayer = true
	}

	// DD-TODO : add frame to forwarded queue if entire frame is forwarded
	// d.logger.Debugw("select packet", "target", currentTarget, "layer", s.layer)

	ddExtension := &dd.DependencyDescriptorExtension{
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

	// RAJA-TODO: this should check for current layer and not target layer
	mark := extPkt.Packet.Header.Marker || (extPkt.DependencyDescriptor.LastPacketInFrame && d.targetLayer.Spatial == int32(extPkt.DependencyDescriptor.FrameDependencies.SpatialId))
	result.RTPMarker = mark
	result.IsSelected = true
	return
}

func (d *DependencyDescriptor) SetTarget(targetLayer buffer.VideoLayer) {
	d.Base.SetTarget(targetLayer)

	activeBitMask := uint32(0)
	var maxSpatial, maxTemporal int32
	for _, dt := range d.decodeTargetLayer {
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
	d.logger.Debugw("setting target", "targetlayer", targetLayer, "activeDecodeTargetsBitmask", d.activeDecodeTargetsBitmask)
}

func (d *DependencyDescriptor) updateDependencyStructure(structure *dd.FrameDependencyStructure) {
	d.structure = structure
	d.decodeTargetLayer = d.decodeTargetLayer[:0]

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
		d.decodeTargetLayer = append(d.decodeTargetLayer, targetLayer{target, layer})
	}

	// sort decode target layer by spatial and temporal from high to low
	sort.Slice(d.decodeTargetLayer, func(i, j int) bool {
		if d.decodeTargetLayer[i].Layer.Spatial == d.decodeTargetLayer[j].Layer.Spatial {
			return d.decodeTargetLayer[i].Layer.Temporal > d.decodeTargetLayer[j].Layer.Temporal
		}
		return d.decodeTargetLayer[i].Layer.Spatial > d.decodeTargetLayer[j].Layer.Spatial
	})
	d.logger.Debugw(fmt.Sprintf("update decode targets: %v", d.decodeTargetLayer))
}

// DD-TODO : use generic wrapper when updated to go 1.18
type Uint16Wrapper struct {
	lastValue     *uint16
	lastUnwrapped int32
}

func (w *Uint16Wrapper) Unwrap(value uint16) int32 {
	if w.lastValue == nil {
		w.lastValue = &value
		w.lastUnwrapped = int32(value)
		return int32(*w.lastValue)
	}

	diff := value - *w.lastValue
	w.lastUnwrapped += int32(diff)
	if diff == 0x8000 && value < *w.lastValue {
		w.lastUnwrapped -= 0x10000
	} else if diff > 0x8000 {
		w.lastUnwrapped -= 0x10000
	}

	*w.lastValue = value
	return w.lastUnwrapped
}
