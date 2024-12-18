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

package buffer

import (
	"fmt"
	"sort"

	"github.com/pion/rtp"
	"go.uber.org/atomic"

	dd "github.com/livekit/livekit-server/pkg/sfu/rtpextension/dependencydescriptor"
	"github.com/livekit/livekit-server/pkg/sfu/utils"

	"github.com/livekit/protocol/logger"
)

var (
	ErrFrameEarlierThanKeyFrame            = fmt.Errorf("frame is earlier than current keyframe")
	ErrDDStructureAttachedToNonFirstPacket = fmt.Errorf("dependency descriptor structure is attached to non-first packet of a frame")
)

type DependencyDescriptorParser struct {
	structure         *dd.FrameDependencyStructure
	ddExtID           uint8
	logger            logger.Logger
	onMaxLayerChanged func(int32, int32)
	decodeTargets     []DependencyDescriptorDecodeTarget

	seqWrapAround             *utils.WrapAround[uint16, uint64]
	frameWrapAround           *utils.WrapAround[uint16, uint64]
	structureExtFrameNum      uint64
	activeDecodeTargetsExtSeq uint64
	activeDecodeTargetsMask   uint32
	frameChecker              *FrameIntegrityChecker

	ddNotFoundCount atomic.Uint32
}

func NewDependencyDescriptorParser(ddExtID uint8, logger logger.Logger, onMaxLayerChanged func(int32, int32)) *DependencyDescriptorParser {
	return &DependencyDescriptorParser{
		ddExtID:           ddExtID,
		logger:            logger,
		onMaxLayerChanged: onMaxLayerChanged,
		seqWrapAround:     utils.NewWrapAround[uint16, uint64](utils.WrapAroundParams{IsRestartAllowed: false}),
		frameWrapAround:   utils.NewWrapAround[uint16, uint64](utils.WrapAroundParams{IsRestartAllowed: false}),
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
	// the frame number of the keyframe which the current frame depends on
	ExtKeyFrameNum uint64
}

func (r *DependencyDescriptorParser) Parse(pkt *rtp.Packet) (*ExtDependencyDescriptor, VideoLayer, error) {
	var videoLayer VideoLayer
	ddBuf := pkt.GetExtension(r.ddExtID)
	if ddBuf == nil {
		ddNotFoundCount := r.ddNotFoundCount.Inc()
		if ddNotFoundCount%100 == 0 {
			r.logger.Warnw("dependency descriptor extension is not present", nil, "seq", pkt.SequenceNumber, "count", ddNotFoundCount)
		}
		return nil, videoLayer, nil
	}

	var ddVal dd.DependencyDescriptor
	ext := &dd.DependencyDescriptorExtension{
		Descriptor: &ddVal,
		Structure:  r.structure,
	}
	_, err := ext.Unmarshal(ddBuf)
	if err != nil {
		if err != dd.ErrDDReaderNoStructure && err != dd.ErrDDReaderInvalidTemplateIndex {
			r.logger.Infow("failed to parse generic dependency descriptor", err, "payload", pkt.PayloadType, "ddbufLen", len(ddBuf))
		}
		return nil, videoLayer, err
	}

	extSeq := r.seqWrapAround.Update(pkt.SequenceNumber).ExtendedVal

	if ddVal.FrameDependencies != nil {
		videoLayer.Spatial, videoLayer.Temporal = int32(ddVal.FrameDependencies.SpatialId), int32(ddVal.FrameDependencies.TemporalId)
	}

	unwrapped := r.frameWrapAround.Update(ddVal.FrameNumber)
	extFN := unwrapped.ExtendedVal

	if extFN < r.structureExtFrameNum {
		r.logger.Debugw("drop frame which is earlier than current structure", "frameNum", extFN, "structureFrameNum", r.structureExtFrameNum)
		return nil, videoLayer, ErrFrameEarlierThanKeyFrame
	}

	r.frameChecker.AddPacket(extSeq, extFN, &ddVal)

	extDD := &ExtDependencyDescriptor{
		Descriptor:  &ddVal,
		ExtFrameNum: extFN,
		Integrity:   r.frameChecker.FrameIntegrity(extFN),
	}

	if ddVal.AttachedStructure != nil {
		if !ddVal.FirstPacketInFrame {
			r.logger.Warnw("attached structure is not the first packet in frame", nil, "extSeq", extSeq, "extFN", extFN)
			return nil, videoLayer, ErrDDStructureAttachedToNonFirstPacket
		}

		if r.structure == nil || ddVal.AttachedStructure.StructureId != r.structure.StructureId {
			r.logger.Debugw("structure updated", "structureID", ddVal.AttachedStructure.StructureId, "extSeq", extSeq, "extFN", extFN, "descriptor", ddVal.String())
		}
		r.structure = ddVal.AttachedStructure
		r.decodeTargets = ProcessFrameDependencyStructure(ddVal.AttachedStructure)
		if extFN > unwrapped.PreExtendedHighest && extFN-unwrapped.PreExtendedHighest > 1000 {
			r.logger.Debugw("large frame number jump on structure updating", "extFN", extFN, "preExtendedHighest", unwrapped.PreExtendedHighest, "structureExtFrameNum", r.structureExtFrameNum)
		}
		r.structureExtFrameNum = extFN
		extDD.StructureUpdated = true
		extDD.ActiveDecodeTargetsUpdated = true
		// The dependency descriptor reader will always set ActiveDecodeTargetsBitmask for TemplateDependencyStructure is present,
		// so don't need to notify max layer change here.
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
	extDD.ExtKeyFrameNum = r.structureExtFrameNum

	return extDD, videoLayer, nil
}

// ------------------------------------------------------------------------------

type DependencyDescriptorDecodeTarget struct {
	Target int
	Layer  VideoLayer
}

func (dt *DependencyDescriptorDecodeTarget) String() string {
	return fmt.Sprintf("DecodeTarget{t: %d, l: %+v}", dt.Target, dt.Layer)
}

// ------------------------------------------------------------------------------

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
