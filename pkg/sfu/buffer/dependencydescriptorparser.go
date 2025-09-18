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
	"time"

	"github.com/pion/rtp"
	"go.uber.org/atomic"

	dd "github.com/livekit/livekit-server/pkg/sfu/rtpextension/dependencydescriptor"
	"github.com/livekit/livekit-server/pkg/sfu/utils"

	"github.com/livekit/protocol/logger"
)

const (
	ddRestartThreshold = 30 * time.Second

	// frame integrity check 2 seconds for L3T3 30fps video
	integrityCheckFrame = 180
	integrityCheckPkt   = 1024
)

var (
	ErrFrameEarlierThanKeyFrame            = fmt.Errorf("frame is earlier than current keyframe")
	ErrDDStructureAttachedToNonFirstPacket = fmt.Errorf("dependency descriptor structure is attached to non-first packet of a frame")
	ErrDDExtentionNotFound                 = fmt.Errorf("dependency descriptor extension not found")
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

	// restart detection
	restartGeneration int
	enableRestart     bool
	lastPacketAt      time.Time
}

func NewDependencyDescriptorParser(ddExtID uint8, logger logger.Logger, onMaxLayerChanged func(int32, int32), enableRestart bool) *DependencyDescriptorParser {
	return &DependencyDescriptorParser{
		ddExtID:           ddExtID,
		logger:            logger,
		onMaxLayerChanged: onMaxLayerChanged,
		seqWrapAround:     utils.NewWrapAround[uint16, uint64](utils.WrapAroundParams{IsRestartAllowed: false}),
		frameWrapAround:   utils.NewWrapAround[uint16, uint64](utils.WrapAroundParams{IsRestartAllowed: false}),
		frameChecker:      NewFrameIntegrityChecker(integrityCheckFrame, integrityCheckPkt),
		enableRestart:     enableRestart,
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

	// increase when the stream restarts, clear and reinitialize all dd state includes
	// attached structure, frame chain, decode target.
	RestartGeneration int
}

func (r *DependencyDescriptorParser) Parse(pkt *rtp.Packet) (*ExtDependencyDescriptor, VideoLayer, error) {
	var videoLayer VideoLayer
	ddBuf := pkt.GetExtension(r.ddExtID)
	if ddBuf == nil {
		ddNotFoundCount := r.ddNotFoundCount.Inc()
		if ddNotFoundCount%100 == 0 {
			r.logger.Warnw("dependency descriptor extension is not present", nil, "seq", pkt.SequenceNumber, "count", ddNotFoundCount)
		}
		return nil, videoLayer, ErrDDExtentionNotFound
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

	var restart bool
	if r.enableRestart {
		if !r.lastPacketAt.IsZero() && time.Since(r.lastPacketAt) > ddRestartThreshold {
			r.restart()
			restart = true
			r.logger.Debugw(
				"dependency descriptor parser restart stream",
				"generation", r.restartGeneration,
				"lastPacketAt", r.lastPacketAt,
				"sinceLast", time.Since(r.lastPacketAt),
				"frameWrapAround", r.frameWrapAround,
			)
		}
		r.lastPacketAt = time.Now()
	}

	extSeq := r.seqWrapAround.Update(pkt.SequenceNumber).ExtendedVal

	if ddVal.FrameDependencies != nil {
		videoLayer.Spatial, videoLayer.Temporal = int32(ddVal.FrameDependencies.SpatialId), int32(ddVal.FrameDependencies.TemporalId)
	}

	// assume the packet is in-order when stream restarting
	unwrapped := r.frameWrapAround.UpdateWithOrderKnown(ddVal.FrameNumber, restart)
	extFN := unwrapped.ExtendedVal

	if extFN < r.structureExtFrameNum {
		r.logger.Debugw(
			"drop frame which is earlier than current structure",
			"fn", ddVal.FrameNumber,
			"extFN", extFN,
			"structureExtFrameNum", r.structureExtFrameNum,
			"unwrappedFN", unwrapped,
			"frameWrapAround", r.frameWrapAround,
		)
		return nil, videoLayer, ErrFrameEarlierThanKeyFrame
	}

	r.frameChecker.AddPacket(extSeq, extFN, &ddVal)

	extDD := &ExtDependencyDescriptor{
		Descriptor:        &ddVal,
		ExtFrameNum:       extFN,
		Integrity:         r.frameChecker.FrameIntegrity(extFN),
		RestartGeneration: r.restartGeneration,
	}

	if ddVal.AttachedStructure != nil {
		if !ddVal.FirstPacketInFrame {
			r.logger.Warnw(
				"attached structure is not the first packet in frame", nil,
				"sn", pkt.SequenceNumber,
				"extSeq", extSeq,
				"fn", ddVal.FrameNumber,
				"extFN", extFN,
			)
			return nil, videoLayer, ErrDDStructureAttachedToNonFirstPacket
		}

		if r.structure == nil || ddVal.AttachedStructure.StructureId != r.structure.StructureId {
			r.logger.Debugw(
				"structure updated",
				"structureID", ddVal.AttachedStructure.StructureId,
				"sn", pkt.SequenceNumber,
				"extSeq", extSeq,
				"fn", ddVal.FrameNumber,
				"extFN", extFN,
				"descriptor", ddVal.String(),
				"unwrappedFN", unwrapped,
				"frameWrapAround", r.frameWrapAround,
			)
		}
		r.structure = ddVal.AttachedStructure
		r.decodeTargets = ProcessFrameDependencyStructure(ddVal.AttachedStructure)
		if extFN > unwrapped.PreExtendedHighest && extFN-unwrapped.PreExtendedHighest > 1000 {
			r.logger.Debugw(
				"large frame number jump on structure updating",
				"fn", ddVal.FrameNumber,
				"extFN", extFN,
				"preExtendedHighest", unwrapped.PreExtendedHighest,
				"structureExtFrameNum", r.structureExtFrameNum,
				"unwrappedFN", unwrapped,
				"frameWrapAround", r.frameWrapAround,
			)
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

func (r *DependencyDescriptorParser) restart() {
	r.frameChecker = NewFrameIntegrityChecker(integrityCheckFrame, integrityCheckPkt)
	r.structure = nil
	r.structureExtFrameNum = 0
	r.activeDecodeTargetsExtSeq = 0
	r.activeDecodeTargetsMask = 0
	r.decodeTargets = r.decodeTargets[:0]
	r.restartGeneration++
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

func ExtractDependencyDescriptorVideoSize(dd *dd.DependencyDescriptor) []VideoSize {
	if dd.AttachedStructure == nil {
		return nil
	}

	videoSizes := make([]VideoSize, 0, len(dd.AttachedStructure.Resolutions))
	for _, res := range dd.AttachedStructure.Resolutions {
		videoSizes = append(videoSizes, VideoSize{Width: uint32(res.Width), Height: uint32(res.Height)})
	}

	return videoSizes
}
