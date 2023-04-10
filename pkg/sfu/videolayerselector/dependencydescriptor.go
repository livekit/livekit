package videolayerselector

import (
	"fmt"
	"sort"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	dd "github.com/livekit/livekit-server/pkg/sfu/dependencydescriptor"
	"github.com/livekit/protocol/logger"
)

type decodeTarget struct {
	Target int
	Layer  buffer.VideoLayer
}

type DependencyDescriptor struct {
	*Base

	// DD-TODO : fields for frame chain detect
	// frameNumberWrapper Uint16Wrapper
	// expectKeyFrame      bool

	decodeTargets              []decodeTarget
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

func (d *DependencyDescriptor) IsOvershootOkay() bool {
	return false
}

func (d *DependencyDescriptor) Select(extPkt *buffer.ExtPacket, _layer int32) (result VideoLayerSelectorResult) {
	if extPkt.DependencyDescriptor == nil {
		// packet don't have dependency descriptor
		return
	}

	if !d.currentLayer.IsValid() && !extPkt.KeyFrame {
		return
	}

	result.IsRelevant = true

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
	for _, dt := range d.decodeTargets {
		// find target match with selected layer
		if dt.Layer.Spatial <= d.targetLayer.Spatial && dt.Layer.Temporal <= d.targetLayer.Temporal {
			if activeDecodeTargets == nil || ((*activeDecodeTargets)&(1<<dt.Target) != 0) {
				// DD-TODO : check frame chain integrity
				currentTarget = dt.Target
				// d.logger.Debugw("select target", "target", currentTarget, "layer", dt.Target, "dtis", extPkt.DependencyDescriptor.FrameDependencies.DecodeTargetIndications)
				break
			}
		}
	}

	if currentTarget < 0 {
		//d.logger.Debugw(fmt.Sprintf("drop packet for no target found, decodeTargets %v, tagetLayer %v, s:%d, t:%d",
		//d.decodeTargets,
		//d.targetLayer,
		//extPkt.DependencyDescriptor.FrameDependencies.SpatialId,
		//extPkt.DependencyDescriptor.FrameDependencies.TemporalId,
		//))

		// no active decode target, do not select
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
	dti := dtis[currentTarget]
	if dti == dd.DecodeTargetNotPresent {
		//d.logger.Debugw(fmt.Sprintf("drop packet for decode target not present, dtis %v, currentTarget %d, s:%d, t:%d",
		//dtis,
		//currentTarget,
		//extPkt.DependencyDescriptor.FrameDependencies.SpatialId,
		//extPkt.DependencyDescriptor.FrameDependencies.TemporalId,
		//))
		return
	}

	if dti == dd.DecodeTargetSwitch {
		// dependency descriptor decode target switch is enabled at all potential switch points.
		// So, setting current layer on every switch point will change current layer a lot.
		//
		// However `currentLayer` is not needed for layer selection in this selector.
		// But, it is needed to signal things in the selector checks outside of this selector.
		//
		// The following cases are handled
		//   1. To detect resumption
		//   2. To detect target achieved so that key frame requests can be stopped
		//   3. To detect reaching max spatial layer - checked when current hits target
		if !d.currentLayer.IsValid() {
			result.IsResuming = true

			d.currentLayer = buffer.VideoLayer{
				Spatial:  int32(extPkt.DependencyDescriptor.FrameDependencies.SpatialId),
				Temporal: int32(extPkt.DependencyDescriptor.FrameDependencies.TemporalId),
			}

			d.logger.Infow(
				"resuming at layer",
				"current", d.currentLayer,
				"target", d.targetLayer,
				"max", d.maxLayer,
				"layer", extPkt.DependencyDescriptor.FrameDependencies.SpatialId,
				"req", d.requestSpatial,
				"maxSeen", d.maxSeenLayer,
				"feed", extPkt.Packet.SSRC,
			)
		}

		if d.currentLayer != d.targetLayer {
			if d.currentLayer.Spatial != d.targetLayer.Spatial && int32(extPkt.DependencyDescriptor.FrameDependencies.SpatialId) == d.targetLayer.Spatial {
				d.currentLayer.Spatial = d.targetLayer.Spatial

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
						"layer", extPkt.DependencyDescriptor.FrameDependencies.SpatialId,
						"req", d.requestSpatial,
						"maxSeen", d.maxSeenLayer,
						"feed", extPkt.Packet.SSRC,
					)
				}
			}

			if d.currentLayer.Temporal != d.targetLayer.Temporal && int32(extPkt.DependencyDescriptor.FrameDependencies.TemporalId) == d.targetLayer.Temporal {
				d.currentLayer.Temporal = d.targetLayer.Temporal
			}
		}
	}

	// DD-TODO : add frame to forwarded queue if entire frame is forwarded
	// d.logger.Debugw("select packet", "target", currentTarget, "targetLayer", d.targetLayer)

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

	result.RTPMarker = extPkt.Packet.Header.Marker || (extPkt.DependencyDescriptor.LastPacketInFrame && d.targetLayer.Spatial == int32(extPkt.DependencyDescriptor.FrameDependencies.SpatialId))
	result.IsSelected = true
	return
}

func (d *DependencyDescriptor) SetTarget(targetLayer buffer.VideoLayer) {
	d.Base.SetTarget(targetLayer)

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
	d.logger.Debugw("setting target", "targetlayer", targetLayer, "activeDecodeTargetsBitmask", d.activeDecodeTargetsBitmask)
}

func (d *DependencyDescriptor) updateDependencyStructure(structure *dd.FrameDependencyStructure) {
	d.structure = structure
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
	d.logger.Debugw(fmt.Sprintf("update decode targets: %v", d.decodeTargets))
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
