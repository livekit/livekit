package sfu

import (
	"fmt"
	"sort"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	dd "github.com/livekit/livekit-server/pkg/sfu/dependencydescriptor"
	"github.com/livekit/protocol/logger"
)

type targetLayer struct {
	Target int
	Layer  VideoLayers
}

type DDVideoLayerSelector struct {
	logger logger.Logger

	// TODO : fields for frame chain detect
	// frameNumberWrapper Uint16Wrapper
	// expectKeyFrame      bool

	decodeTargetLayer          []targetLayer
	layer                      VideoLayers
	activeDecodeTargetsBitmask *uint32
	structure                  *dd.FrameDependencyStructure
}

func NewDDVideoLayerSelector(logger logger.Logger) *DDVideoLayerSelector {
	return &DDVideoLayerSelector{
		logger: logger,
		layer:  VideoLayers{Spatial: 2, Temporal: 2},
	}
}

func (s *DDVideoLayerSelector) Select(expPkt *buffer.ExtPacket, tp *TranslationParams) (selected bool) {
	// return true
	tp.marker = expPkt.Packet.Marker
	if expPkt.DependencyDescriptor == nil {
		// packet don't have dependency descriptor, pass check
		return true
	}

	if expPkt.DependencyDescriptor.AttachedStructure != nil {
		// update decode target layer and active decode targets
		// TODO : these targets info can be shared by all the downtracks, no need calculate in every selector
		s.updateDependencyStructure(expPkt.DependencyDescriptor.AttachedStructure)
	}

	// forward all packets before locking
	if s.layer == InvalidLayers {
		return true
	}

	// TODO : we don't have a rtp queue to ensure the order of packets now,
	// so we don't know packet is lost/out of order, that cause us can't detect
	// frame integrity, entire frame is forwareded, whether frame chain is broken.
	// So use a simple check here, assume all the reference frame is forwarded and
	// only check DTI of the active decode target.
	// it is not effeciency, at last we need check frame chain integrity.

	activeDecodeTargets := expPkt.DependencyDescriptor.ActiveDecodeTargetsBitmask
	if activeDecodeTargets != nil {
		s.logger.Debugw("active decode targets", "activeDecodeTargets", *activeDecodeTargets)
	}

	currentTarget := -1
	for _, dt := range s.decodeTargetLayer {
		// find target match with selected layer
		if dt.Layer.Spatial <= s.layer.Spatial && dt.Layer.Temporal <= s.layer.Temporal {
			if activeDecodeTargets == nil || ((*activeDecodeTargets)&(1<<dt.Target) != 0) {
				// TODO : check frame chain integrity
				currentTarget = dt.Target
				// s.logger.Debugw("select target", "target", currentTarget, "layer", dt.layer, "dtis", expPkt.DependencyDescriptor.FrameDependencies.DecodeTargetIndications)
				break
			}
		}
	}

	if currentTarget < 0 {
		// s.logger.Debugw(fmt.Sprintf("drop packet for no target found, deocdeTargets %v, selected layer %v, s:%d, t:%d",
		// s.decodeTargetLayer, s.layer, expPkt.DependencyDescriptor.FrameDependencies.SpatialId, expPkt.DependencyDescriptor.FrameDependencies.TemporalId))
		// no active decode target, forward all packets
		return false
	}

	dtis := expPkt.DependencyDescriptor.FrameDependencies.DecodeTargetIndications
	if len(dtis) < currentTarget {
		// dtis error, dependency descriptor might lost
		s.logger.Debugw(fmt.Sprintf("drop packet for dtis error, dtis %v, currentTarget %d, s:%d, t:%d", dtis, currentTarget,
			expPkt.DependencyDescriptor.FrameDependencies.SpatialId, expPkt.DependencyDescriptor.FrameDependencies.TemporalId))
		return false
	}

	// TODO : if bandwidth in congest, could drop the 'Discardable' packet
	if dti := dtis[currentTarget]; dti == dd.DecodeTargetNotPresent {
		// s.logger.Debugw(fmt.Sprintf("drop packet for decode target not present, dtis %v, currentTarget %d, s:%d, t:%d", dtis, currentTarget,
		// expPkt.DependencyDescriptor.FrameDependencies.SpatialId, expPkt.DependencyDescriptor.FrameDependencies.TemporalId))
		return false
	} else if dti == dd.DecodeTargetSwitch {
		tp.switchingToTargetLayer = true
	}

	// TODO : add frame to forwarded queue if entire frame is forwarded
	// s.logger.Debugw("select packet", "target", currentTarget, "layer", s.layer)

	tp.ddExtension = &dd.DependencyDescriptorExtension{
		Descriptor: expPkt.DependencyDescriptor,
		Structure:  s.structure,
	}
	if expPkt.DependencyDescriptor.AttachedStructure == nil && s.activeDecodeTargetsBitmask != nil {
		// clone and override activebitmask
		ddClone := *tp.ddExtension.Descriptor
		ddClone.ActiveDecodeTargetsBitmask = s.activeDecodeTargetsBitmask
		tp.ddExtension.Descriptor = &ddClone
		// s.logger.Debugw("set active decode targets bitmask", "activeDecodeTargetsBitmask", s.activeDecodeTargetsBitmask)
	}

	mark := expPkt.Packet.Header.Marker || (expPkt.DependencyDescriptor.LastPacketInFrame && s.layer.Spatial == int32(expPkt.DependencyDescriptor.FrameDependencies.SpatialId))
	tp.marker = mark

	return true
}

func (s *DDVideoLayerSelector) SelectLayer(layer VideoLayers) {
	// layer = VideoLayers{1, 1}
	s.layer = layer
	activeBitMask := uint32(0)
	var maxSpatial, maxTemporal int32
	for _, dt := range s.decodeTargetLayer {
		if dt.Layer.Spatial > maxSpatial {
			maxSpatial = dt.Layer.Spatial
		}
		if dt.Layer.Temporal > maxTemporal {
			maxTemporal = dt.Layer.Temporal
		}
		if dt.Layer.Spatial <= layer.Spatial && dt.Layer.Temporal <= layer.Temporal {
			activeBitMask |= 1 << dt.Target
		}
	}
	if layer.Spatial == maxSpatial && layer.Temporal == maxTemporal {
		// all the decode targets are selected
		s.activeDecodeTargetsBitmask = nil
	} else {
		s.activeDecodeTargetsBitmask = &activeBitMask
	}
	s.logger.Debugw("select layer ", "layer", layer, "activeDecodeTargetsBitmask", s.activeDecodeTargetsBitmask)
}

func (s *DDVideoLayerSelector) updateDependencyStructure(structure *dd.FrameDependencyStructure) {
	s.structure = structure
	s.decodeTargetLayer = s.decodeTargetLayer[:0]

	for target := 0; target < structure.NumDecodeTargets; target++ {
		layer := VideoLayers{Spatial: 0, Temporal: 0}
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
		s.decodeTargetLayer = append(s.decodeTargetLayer, targetLayer{target, layer})
	}

	// sort decode target layer by spatial and temporal from high to low
	sort.Slice(s.decodeTargetLayer, func(i, j int) bool {
		if s.decodeTargetLayer[i].Layer.Spatial == s.decodeTargetLayer[j].Layer.Spatial {
			return s.decodeTargetLayer[i].Layer.Temporal > s.decodeTargetLayer[j].Layer.Temporal
		}
		return s.decodeTargetLayer[i].Layer.Spatial > s.decodeTargetLayer[j].Layer.Spatial
	})
	s.logger.Debugw(fmt.Sprintf("update decode targets: %v", s.decodeTargetLayer))
}

// TODO : use generic wrapper when updated to go 1.18
type Uint16Wrapper struct {
	last_value    *uint16
	lastUnwrapped int32
}

func (w *Uint16Wrapper) Unwrap(value uint16) int32 {
	if w.last_value == nil {
		w.last_value = &value
		w.lastUnwrapped = int32(value)
		return int32(*w.last_value)
	}

	diff := value - *w.last_value
	w.lastUnwrapped += int32(diff)
	if diff == 0x8000 && value < *w.last_value {
		w.lastUnwrapped -= 0x10000
	} else if diff > 0x8000 {
		w.lastUnwrapped -= 0x10000
	}

	*w.last_value = value
	return w.lastUnwrapped
}
