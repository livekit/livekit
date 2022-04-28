package sfu

import (
	"fmt"
	"sort"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	dd "github.com/livekit/livekit-server/pkg/sfu/buffer/dependencydescriptor"
	"github.com/livekit/protocol/logger"
)

type targetLayer struct {
	target int
	layer  VideoLayers
}

func (t targetLayer) String() string {
	return fmt.Sprintf("target:%d, layer:%s", t.target, t.layer)
}

type VideoLayerSelector struct {
	logger logger.Logger
	// frameNumberWrapper Uint16Wrapper
	// expectKeyFrame      bool
	decodeTargetLayer          []targetLayer
	layer                      VideoLayers
	activeDecodeTargetsBitmask *uint32
	structure                  *dd.FrameDependencyStructure
}

func NewVideoLayerSelector(logger logger.Logger) *VideoLayerSelector {
	return &VideoLayerSelector{
		logger: logger,
		layer:  VideoLayers{2, 2},
	}
}

func (s *VideoLayerSelector) Select(expPkt *buffer.ExtPacket) (selected, marker bool, ddExt *dd.DependencyDescriptorExtension) {
	// return true
	if expPkt.DependencyDescriptor == nil {
		// packet don't have dependency descriptor, pass check
		return true, expPkt.Packet.Marker, nil
	}

	if expPkt.DependencyDescriptor.AttachedStructure != nil {
		// update decode target layer and active decode targets
		// TODO : these two can be shared by all the downtracks, no need calculate in every selector
		s.updateDependencyStructure(expPkt.DependencyDescriptor.AttachedStructure)
	}

	// forward all packets before locking
	if s.layer == InvalidLayers {
		return true, expPkt.Packet.Marker, nil
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
		if dt.layer.spatial <= s.layer.spatial && dt.layer.temporal <= s.layer.temporal {
			if activeDecodeTargets == nil || ((*activeDecodeTargets)&(1<<dt.target) != 0) {
				// TODO : check frame chain integrity
				currentTarget = dt.target
				// s.logger.Debugw("select target", "target", currentTarget, "layer", dt.layer, "dtis", expPkt.DependencyDescriptor.FrameDependencies.DecodeTargetIndications)
				break
			}
		}
	}

	if currentTarget < 0 {
		s.logger.Debugw(fmt.Sprintf("drop packet for no target found, deocdeTargets %v, selected layer %v, s:%d, t:%d",
			s.decodeTargetLayer, s.layer, expPkt.DependencyDescriptor.FrameDependencies.SpatialId, expPkt.DependencyDescriptor.FrameDependencies.TemporalId))
		// no active decode target, forward all packets
		return false, false, nil
	}

	dtis := expPkt.DependencyDescriptor.FrameDependencies.DecodeTargetIndications
	if len(dtis) < currentTarget {
		// dtis error, dependency descriptor might lost
		s.logger.Debugw(fmt.Sprintf("drop packet for dtis error, dtis %v, currentTarget %d, s:%d, t:%d", dtis, currentTarget,
			expPkt.DependencyDescriptor.FrameDependencies.SpatialId, expPkt.DependencyDescriptor.FrameDependencies.TemporalId))
		return false, false, nil
	}

	// TODO : if bandwidth in congest, could drop the 'Discardable' packet
	if dtis[currentTarget] == dd.DecodeTargetNotPresent {
		// s.logger.Debugw(fmt.Sprintf("drop packet for decode target not present, dtis %v, currentTarget %d, s:%d, t:%d", dtis, currentTarget,
		// expPkt.DependencyDescriptor.FrameDependencies.SpatialId, expPkt.DependencyDescriptor.FrameDependencies.TemporalId))
		return false, false, nil
	}

	// TODO : add frame to forwarded queue if entire frame is forwarded
	// s.logger.Debugw("select packet", "target", currentTarget, "layer", s.layer)

	if expPkt.DependencyDescriptor.AttachedStructure == nil {
		expPkt.DependencyDescriptor.ActiveDecodeTargetsBitmask = s.activeDecodeTargetsBitmask
		s.logger.Debugw("set active decode targets bitmask", "activeDecodeTargetsBitmask", s.activeDecodeTargetsBitmask)
	}

	mark := expPkt.Packet.Header.Marker || (expPkt.DependencyDescriptor.LastPacketInFrame && s.layer.spatial == int32(expPkt.DependencyDescriptor.FrameDependencies.SpatialId))
	return true, mark, &dd.DependencyDescriptorExtension{
		Descriptor: expPkt.DependencyDescriptor,
		Structure:  s.structure,
	}
}

func (s *VideoLayerSelector) SelectLayer(layer VideoLayers) {
	// time.AfterFunc(time.Duration(5)*time.Second, func() {
	layer = VideoLayers{1, 1}
	s.layer = layer
	activeBitMask := uint32(0)
	var maxSpatial, maxTemporal int32
	for _, dt := range s.decodeTargetLayer {
		if dt.layer.spatial > maxSpatial {
			maxSpatial = dt.layer.spatial
		}
		if dt.layer.temporal > maxTemporal {
			maxTemporal = dt.layer.temporal
		}
		if dt.layer.spatial <= layer.spatial && dt.layer.temporal <= layer.temporal {

			activeBitMask |= 1 << dt.target
		}
	}
	if layer.spatial == maxSpatial && layer.temporal == maxTemporal {
		// all the decode targets are selected
		s.activeDecodeTargetsBitmask = nil
	} else {
		s.activeDecodeTargetsBitmask = &activeBitMask
	}
	s.logger.Debugw("select layer ", "layer", layer, "activeDecodeTargetsBitmask", s.activeDecodeTargetsBitmask)
	// })

}

func (s *VideoLayerSelector) updateDependencyStructure(structure *dd.FrameDependencyStructure) {
	s.structure = structure
	s.decodeTargetLayer = s.decodeTargetLayer[:0]

	for target := 0; target < structure.NumDecodeTargets; target++ {
		layer := VideoLayers{0, 0}
		for _, t := range structure.Templates {
			if t.DecodeTargetIndications[target] != dd.DecodeTargetNotPresent {
				if layer.spatial < int32(t.SpatialId) {
					layer.spatial = int32(t.SpatialId)
				}
				if layer.temporal < int32(t.TemporalId) {
					layer.temporal = int32(t.TemporalId)
				}
			}
		}
		s.decodeTargetLayer = append(s.decodeTargetLayer, targetLayer{target, layer})
		s.logger.Debugw("append decode target", "target", target, "layer", layer)
	}

	// sort decode target layer by spatial and temporal from high to low
	sort.Slice(s.decodeTargetLayer, func(i, j int) bool {
		if s.decodeTargetLayer[i].layer.spatial == s.decodeTargetLayer[j].layer.spatial {
			return s.decodeTargetLayer[i].layer.temporal > s.decodeTargetLayer[j].layer.temporal
		}
		return s.decodeTargetLayer[i].layer.spatial > s.decodeTargetLayer[j].layer.spatial
	})

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
