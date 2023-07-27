package buffer

import (
	"fmt"
	"sort"

	"github.com/pion/rtp"

	dd "github.com/livekit/livekit-server/pkg/sfu/dependencydescriptor"
	"github.com/livekit/livekit-server/pkg/sfu/utils"

	"github.com/livekit/protocol/logger"
)

type DependencyDescriptorParser struct {
	structure         *dd.FrameDependencyStructure
	ddExtID           uint8
	logger            logger.Logger
	onMaxLayerChanged func(int32, int32)
	decodeTargets     []DependencyDescriptorDecodeTarget

	seqWrapAround             *utils.WrapAround[uint16, uint64]
	frameWrapAround           *utils.WrapAround[uint16, uint64]
	structureExtSeq           uint64
	activeDecodeTargetsExtSeq uint64
	activeDecodeTargetsMask   uint32
	frameChecker              *FrameIntegrityChecker
}

func NewDependencyDescriptorParser(ddExtID uint8, logger logger.Logger, onMaxLayerChanged func(int32, int32)) *DependencyDescriptorParser {
	logger.Infow("creating dependency descriptor parser", "ddExtID", ddExtID)
	return &DependencyDescriptorParser{
		ddExtID:           ddExtID,
		logger:            logger,
		onMaxLayerChanged: onMaxLayerChanged,
		seqWrapAround:     utils.NewWrapAround[uint16, uint64](),
		frameWrapAround:   utils.NewWrapAround[uint16, uint64](),
		frameChecker:      NewFrameIntegrityChecker(180, 1024), // 2seconds for L3T3 30fps video
	}
}

type ExtDependencyDescriptor struct {
	Descriptor *dd.DependencyDescriptor

	DecodeTargets              []DependencyDescriptorDecodeTarget
	StructureUpdated           bool
	ActiveDecodeTargetsUpdated bool
	Integrity                  bool
	ExtFrameNum                uint64
}

func (r *DependencyDescriptorParser) Parse(pkt *rtp.Packet) (*ExtDependencyDescriptor, VideoLayer, error) {
	var videoLayer VideoLayer
	ddBuf := pkt.GetExtension(r.ddExtID)
	if ddBuf == nil {
		return nil, videoLayer, nil
	}

	var ddVal dd.DependencyDescriptor
	ext := &dd.DependencyDescriptorExtension{
		Descriptor: &ddVal,
		Structure:  r.structure,
	}
	_, err := ext.Unmarshal(ddBuf)
	if err != nil {
		// r.logger.Debugw("failed to parse generic dependency descriptor", "err", err, "payload", pkt.PayloadType, "ddbufLen", len(ddBuf))
		return nil, videoLayer, err
	}

	extSeq := r.seqWrapAround.Update(pkt.SequenceNumber).ExtendedVal

	if ddVal.FrameDependencies != nil {
		videoLayer.Spatial, videoLayer.Temporal = int32(ddVal.FrameDependencies.SpatialId), int32(ddVal.FrameDependencies.TemporalId)
	}

	extFN := r.frameWrapAround.Update(ddVal.FrameNumber).ExtendedVal
	r.frameChecker.AddPacket(extSeq, extFN, &ddVal)

	extDD := &ExtDependencyDescriptor{
		Descriptor:  &ddVal,
		ExtFrameNum: extFN,
		Integrity:   r.frameChecker.FrameIntegrity(extFN),
	}

	if ddVal.AttachedStructure != nil {
		r.logger.Debugw(fmt.Sprintf("parsed dependency descriptor\n%s", ddVal.String()))
		if extSeq > r.structureExtSeq {
			r.structure = ddVal.AttachedStructure
			r.decodeTargets = ProcessFrameDependencyStructure(ddVal.AttachedStructure)
			r.structureExtSeq = extSeq
			extDD.StructureUpdated = true
			extDD.ActiveDecodeTargetsUpdated = true
			// The dependency descriptor reader will always set ActiveDecodeTargetsBitmask for TemplateDependencyStructure is present,
			// so don't need to notify max layer change here.
		}
	}

	if mask := ddVal.ActiveDecodeTargetsBitmask; mask != nil && extSeq > r.activeDecodeTargetsExtSeq {
		r.activeDecodeTargetsExtSeq = extSeq
		if *mask != r.activeDecodeTargetsMask {
			r.activeDecodeTargetsMask = *mask
			extDD.ActiveDecodeTargetsUpdated = true
			var maxSpatial, maxTemporal int32
			for _, dt := range r.decodeTargets {
				if *mask&(1<<dt.Target) != uint32(dd.DecodeTargetNotPresent) {
					if maxSpatial < dt.Layer.Spatial {
						maxSpatial = dt.Layer.Spatial
					}
					if maxTemporal < dt.Layer.Temporal {
						maxTemporal = dt.Layer.Temporal
					}
				}
			}
			r.logger.Debugw("max layer changed", "maxSpatial", maxSpatial, "maxTemporal", maxTemporal)
			r.onMaxLayerChanged(maxSpatial, maxTemporal)
		}
	}

	extDD.DecodeTargets = r.decodeTargets

	return extDD, videoLayer, nil
}

// ------------------------------------------------------------------------------

type DependencyDescriptorDecodeTarget struct {
	Target int
	Layer  VideoLayer
}

func ProcessFrameDependencyStructure(structure *dd.FrameDependencyStructure) []DependencyDescriptorDecodeTarget {
	decodeTargets := make([]DependencyDescriptorDecodeTarget, 0, structure.NumDecodeTargets)
	for target := 0; target < structure.NumDecodeTargets; target++ {
		layer := VideoLayer{Spatial: 0, Temporal: 0}
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
		decodeTargets = append(decodeTargets, DependencyDescriptorDecodeTarget{target, layer})
	}

	// sort decode target layer by spatial and temporal from high to low
	sort.Slice(decodeTargets, func(i, j int) bool {
		return decodeTargets[i].Layer.GreaterThan(decodeTargets[j].Layer)
	})

	return decodeTargets
}

func GetActiveDecodeTargetBitmask(layer VideoLayer, decodeTargets []DependencyDescriptorDecodeTarget) *uint32 {
	activeBitMask := uint32(0)
	for _, dt := range decodeTargets {
		if dt.Layer.Spatial <= layer.Spatial && dt.Layer.Temporal <= layer.Temporal {
			activeBitMask |= 1 << dt.Target
		}
	}

	return &activeBitMask
}

// ------------------------------------------------------------------------------
