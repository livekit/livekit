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
	"sync"
	"time"

	"github.com/pion/rtp"

	dd "github.com/livekit/livekit-server/pkg/sfu/rtpextension/dependencydescriptor"
	sutils "github.com/livekit/livekit-server/pkg/utils"
	"github.com/livekit/mediatransportutil/pkg/utils"
	"github.com/livekit/protocol/logger"
)

var (
	ExtDependencyDescriptorFactory = &sync.Pool{
		New: func() any {
			return &ExtDependencyDescriptor{}
		},
	}
)

// --------------------------------------

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

	ddNotFoundLogger sutils.CountedLogger

	// restart detection
	restartGeneration int
	enableRestart     bool
	lastPacketAt      time.Time
}

func NewDependencyDescriptorParser(ddExtID uint8, logger logger.Logger, onMaxLayerChanged func(int32, int32), enableRestart bool) *DependencyDescriptorParser {
	d := &DependencyDescriptorParser{
		ddExtID:           ddExtID,
		logger:            logger,
		onMaxLayerChanged: onMaxLayerChanged,
		seqWrapAround:     utils.NewWrapAround[uint16, uint64](utils.WrapAroundParams{IsRestartAllowed: false}),
		frameWrapAround:   utils.NewWrapAround[uint16, uint64](utils.WrapAroundParams{IsRestartAllowed: false}),
		frameChecker:      NewFrameIntegrityChecker(integrityCheckFrame, integrityCheckPkt),
		enableRestart:     enableRestart,
	}
	d.ddNotFoundLogger = sutils.NewPeriodicLogger(
		d.logger,
		sutils.CountedLoggerLevelWarn,
		sutils.PeriodicLoggerParams{
			Initial: 10,
			Then:    100,
		},
	)
	return d
}

func (d *DependencyDescriptorParser) SetLogger(lgr logger.Logger) {
	d.logger = lgr
	d.ddNotFoundLogger.SetLogger(lgr)
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

func (d *DependencyDescriptorParser) Parse(pkt *rtp.Packet) (*ExtDependencyDescriptor, VideoLayer, error) {
	var videoLayer VideoLayer
	ddBuf := pkt.GetExtension(d.ddExtID)
	if ddBuf == nil {
		d.ddNotFoundLogger.ErrorLog("dependency descriptor extension is not present", nil, "seq", pkt.SequenceNumber)
		return nil, videoLayer, ErrDDExtentionNotFound
	}

	var ddVal dd.DependencyDescriptor
	ext := &dd.DependencyDescriptorExtension{
		Descriptor: &ddVal,
		Structure:  d.structure,
	}
	_, err := ext.Unmarshal(ddBuf)
	if err != nil {
		if err != dd.ErrDDReaderNoStructure && err != dd.ErrDDReaderInvalidTemplateIndex {
			d.logger.Infow("failed to parse generic dependency descriptor", err, "payload", pkt.PayloadType, "ddbufLen", len(ddBuf))
		}
		return nil, videoLayer, err
	}

	var restart bool
	if d.enableRestart {
		if !d.lastPacketAt.IsZero() && time.Since(d.lastPacketAt) > ddRestartThreshold {
			d.restart()
			restart = true
			d.logger.Debugw(
				"dependency descriptor parser restart stream",
				"generation", d.restartGeneration,
				"lastPacketAt", d.lastPacketAt,
				"sinceLast", time.Since(d.lastPacketAt),
				"frameWrapAround", d.frameWrapAround,
			)
		}
		d.lastPacketAt = time.Now()
	}

	extSeq := d.seqWrapAround.Update(pkt.SequenceNumber).ExtendedVal

	if ddVal.FrameDependencies != nil {
		videoLayer.Spatial, videoLayer.Temporal = int32(ddVal.FrameDependencies.SpatialId), int32(ddVal.FrameDependencies.TemporalId)
	}

	// assume the packet is in-order when stream restarting
	unwrapped := d.frameWrapAround.UpdateWithOrderKnown(ddVal.FrameNumber, restart)
	extFN := unwrapped.ExtendedVal

	if extFN < d.structureExtFrameNum {
		d.logger.Debugw(
			"drop frame which is earlier than current structure",
			"fn", ddVal.FrameNumber,
			"extFN", extFN,
			"structureExtFrameNum", d.structureExtFrameNum,
			"unwrappedFN", unwrapped,
			"frameWrapAround", d.frameWrapAround,
		)
		return nil, videoLayer, ErrFrameEarlierThanKeyFrame
	}

	d.frameChecker.AddPacket(extSeq, extFN, &ddVal)

	extDD := ExtDependencyDescriptorFactory.Get().(*ExtDependencyDescriptor)
	*extDD = ExtDependencyDescriptor{
		Descriptor:        &ddVal,
		ExtFrameNum:       extFN,
		Integrity:         d.frameChecker.FrameIntegrity(extFN),
		RestartGeneration: d.restartGeneration,
	}

	if ddVal.AttachedStructure != nil {
		if !ddVal.FirstPacketInFrame {
			d.logger.Warnw(
				"attached structure is not the first packet in frame", nil,
				"sn", pkt.SequenceNumber,
				"extSeq", extSeq,
				"fn", ddVal.FrameNumber,
				"extFN", extFN,
			)
			ReleaseExtDependencyDescriptor(extDD)
			return nil, videoLayer, ErrDDStructureAttachedToNonFirstPacket
		}

		if d.structure == nil || ddVal.AttachedStructure.StructureId != d.structure.StructureId {
			d.logger.Debugw(
				"structure updated",
				"structureID", ddVal.AttachedStructure.StructureId,
				"sn", pkt.SequenceNumber,
				"extSeq", extSeq,
				"fn", ddVal.FrameNumber,
				"extFN", extFN,
				"descriptor", ddVal.String(),
				"unwrappedFN", unwrapped,
				"frameWrapAround", d.frameWrapAround,
			)
		}
		d.structure = ddVal.AttachedStructure
		d.decodeTargets = ProcessFrameDependencyStructure(ddVal.AttachedStructure)
		if extFN > unwrapped.PreExtendedHighest && extFN-unwrapped.PreExtendedHighest > 1000 {
			d.logger.Debugw(
				"large frame number jump on structure updating",
				"fn", ddVal.FrameNumber,
				"extFN", extFN,
				"preExtendedHighest", unwrapped.PreExtendedHighest,
				"structureExtFrameNum", d.structureExtFrameNum,
				"unwrappedFN", unwrapped,
				"frameWrapAround", d.frameWrapAround,
			)
		}
		d.structureExtFrameNum = extFN
		extDD.StructureUpdated = true
		extDD.ActiveDecodeTargetsUpdated = true
		// The dependency descriptor reader will always set ActiveDecodeTargetsBitmask for TemplateDependencyStructure is present,
		// so don't need to notify max layer change here.
	}

	if mask := ddVal.ActiveDecodeTargetsBitmask; mask != nil && extSeq > d.activeDecodeTargetsExtSeq {
		d.activeDecodeTargetsExtSeq = extSeq
		if *mask != d.activeDecodeTargetsMask {
			d.activeDecodeTargetsMask = *mask
			extDD.ActiveDecodeTargetsUpdated = true
			var maxSpatial, maxTemporal int32
			for _, dt := range d.decodeTargets {
				if *mask&(1<<dt.Target) != uint32(dd.DecodeTargetNotPresent) {
					if maxSpatial < dt.Layer.Spatial {
						maxSpatial = dt.Layer.Spatial
					}
					if maxTemporal < dt.Layer.Temporal {
						maxTemporal = dt.Layer.Temporal
					}
				}
			}
			d.logger.Debugw("max layer changed", "maxSpatial", maxSpatial, "maxTemporal", maxTemporal)
			d.onMaxLayerChanged(maxSpatial, maxTemporal)
		}
	}

	extDD.DecodeTargets = d.decodeTargets
	extDD.ExtKeyFrameNum = d.structureExtFrameNum

	return extDD, videoLayer, nil
}

func (d *DependencyDescriptorParser) restart() {
	d.frameChecker = NewFrameIntegrityChecker(integrityCheckFrame, integrityCheckPkt)
	d.structure = nil
	d.structureExtFrameNum = 0
	d.activeDecodeTargetsExtSeq = 0
	d.activeDecodeTargetsMask = 0
	d.decodeTargets = d.decodeTargets[:0]
	d.restartGeneration++
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

// ------------------------------------------------------------------------------

func ReleaseExtDependencyDescriptor(extDD *ExtDependencyDescriptor) {
	if extDD == nil {
		return
	}

	*extDD = ExtDependencyDescriptor{}
	ExtDependencyDescriptorFactory.Put(extDD)
}
