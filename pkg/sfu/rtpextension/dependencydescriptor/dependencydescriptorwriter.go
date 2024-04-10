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

package dependencydescriptor

import (
	"fmt"

	"golang.org/x/exp/slices"
)

type TemplateMatch struct {
	TemplateIdx      int
	NeedCustomDtis   bool
	NeedCustomFdiffs bool
	NeedCustomChains bool
	// Size in bits to store frame-specific details, i.e.
	// excluding mandatory fields and template dependency structure.
	ExtraSizeBits int
}

type DependencyDescriptorWriter struct {
	descriptor   *DependencyDescriptor
	structure    *FrameDependencyStructure
	activeChains uint32
	writer       *BitStreamWriter
	bestTemplate TemplateMatch
}

func NewDependencyDescriptorWriter(buf []byte, structure *FrameDependencyStructure, activeChains uint32, descriptor *DependencyDescriptor) (*DependencyDescriptorWriter, error) {
	writer := NewBitStreamWriter(buf)
	w := &DependencyDescriptorWriter{
		descriptor:   descriptor,
		structure:    structure,
		activeChains: activeChains,
		writer:       writer,
	}
	return w, w.findBestTemplate()
}

func (w *DependencyDescriptorWriter) ResetBuf(buf []byte) {
	w.writer = NewBitStreamWriter(buf)
}

func (w *DependencyDescriptorWriter) Write() error {
	if err := w.findBestTemplate(); err != nil {
		return err
	}

	if err := w.writeMandatoryFields(); err != nil {
		return err
	}

	if w.hasExtendedFields() {
		if err := w.writeExtendedFields(); err != nil {
			return err
		}

		if err := w.writeFrameDependencyDefinition(); err != nil {
			return err
		}
	}

	remainingBits := w.writer.RemainingBits()
	// Zero remaining memory to avoid leaving it uninitialized.
	if remainingBits%64 != 0 {
		if err := w.writeBits(0, remainingBits%64); err != nil {
			return err
		}
	}

	for i := 0; i < remainingBits/64; i++ {
		if err := w.writeBits(0, 64); err != nil {
			return err
		}
	}

	return nil
}

func (w *DependencyDescriptorWriter) findBestTemplate() error {
	// Find templates with same spatial and temporal layer of frame dependency.
	var (
		firstSameLayer                      *FrameDependencyTemplate
		firstSameLayerIdx, lastSameLayerIdx int
	)
	for i, t := range w.structure.Templates {
		if w.descriptor.FrameDependencies.SpatialId == t.SpatialId &&
			w.descriptor.FrameDependencies.TemporalId == t.TemporalId {
			firstSameLayer = t
			firstSameLayerIdx = i
			break
		}
	}

	if firstSameLayer == nil {
		return fmt.Errorf("no template found for spatial layer %d and temporal layer %d", w.descriptor.FrameDependencies.SpatialId, w.descriptor.FrameDependencies.TemporalId)
	}

	for i, t := range w.structure.Templates[firstSameLayerIdx:] {
		if w.descriptor.FrameDependencies.SpatialId != t.SpatialId ||
			w.descriptor.FrameDependencies.TemporalId != t.TemporalId {
			lastSameLayerIdx = i + firstSameLayerIdx
		}
	}

	// Search if there any better template that have small extra size.
	w.bestTemplate = w.calculateMatch(firstSameLayerIdx, firstSameLayer)
	for i := firstSameLayerIdx + 1; i <= lastSameLayerIdx; i++ {
		t := w.structure.Templates[i]
		match := w.calculateMatch(i, t)
		if match.ExtraSizeBits < w.bestTemplate.ExtraSizeBits {
			w.bestTemplate = match
		}
	}
	return nil
}

func (w *DependencyDescriptorWriter) calculateMatch(idx int, template *FrameDependencyTemplate) TemplateMatch {
	var result TemplateMatch
	result.TemplateIdx = idx
	result.NeedCustomFdiffs = w.descriptor.FrameDependencies.FrameDiffs != nil && !slices.Equal(w.descriptor.FrameDependencies.FrameDiffs, template.FrameDiffs)
	result.NeedCustomDtis = w.descriptor.FrameDependencies.DecodeTargetIndications != nil && !slices.Equal(w.descriptor.FrameDependencies.DecodeTargetIndications, template.DecodeTargetIndications)

	for i := 0; i < w.structure.NumChains; i++ {
		if w.activeChains&(1<<i) != 0 && (len(w.descriptor.FrameDependencies.ChainDiffs) <= i || len(template.ChainDiffs) <= i || w.descriptor.FrameDependencies.ChainDiffs[i] != template.ChainDiffs[i]) {
			result.NeedCustomChains = true
			break
		}
	}

	if result.NeedCustomFdiffs {
		result.ExtraSizeBits = 2 * (1 + len(w.descriptor.FrameDependencies.FrameDiffs))

		for _, fdiff := range w.descriptor.FrameDependencies.FrameDiffs {
			if fdiff <= (1 << 4) {
				result.ExtraSizeBits += 4
			} else if fdiff <= (1 << 8) {
				result.ExtraSizeBits += 8
			} else {
				result.ExtraSizeBits += 12
			}
		}
	}

	if result.NeedCustomDtis {
		result.ExtraSizeBits += 2 * len(w.descriptor.FrameDependencies.DecodeTargetIndications)
	}
	if result.NeedCustomChains {
		result.ExtraSizeBits += 8 * w.structure.NumChains
	}

	return result
}

func (w *DependencyDescriptorWriter) writeMandatoryFields() error {
	if err := w.writeBool(w.descriptor.FirstPacketInFrame); err != nil {
		return err
	}

	if err := w.writeBool(w.descriptor.LastPacketInFrame); err != nil {
		return err
	}

	templateId := (w.bestTemplate.TemplateIdx + w.structure.StructureId) % MaxTemplates

	if err := w.writeBits(uint64(templateId), 6); err != nil {
		return err
	}

	return w.writeBits(uint64(w.descriptor.FrameNumber), 16)
}

func (w *DependencyDescriptorWriter) writeBool(val bool) error {
	v := uint64(0)
	if val {
		v = 1
	}
	return w.writer.WriteBits(v, 1)
}

func (w *DependencyDescriptorWriter) writeBits(val uint64, bitCount int) error {
	if err := w.writer.WriteBits(val, bitCount); err != nil {
		return err
	}
	return nil
}

func (w *DependencyDescriptorWriter) hasExtendedFields() bool {
	return w.bestTemplate.ExtraSizeBits > 0 || w.descriptor.AttachedStructure != nil || w.descriptor.ActiveDecodeTargetsBitmask != nil
}

func (w *DependencyDescriptorWriter) writeExtendedFields() error {
	// template_dependency_structure_present_flag
	if err := w.writeBool(w.descriptor.AttachedStructure != nil); err != nil {
		return err
	}

	// active_decode_targets_present_flag
	activeDecodeTargetsPresentFlag := w.shouldWriteActiveDecodeTargetsBitmask()
	if err := w.writeBool(activeDecodeTargetsPresentFlag); err != nil {
		return err
	}

	// need_custom_dtis
	if err := w.writeBool(w.bestTemplate.NeedCustomDtis); err != nil {
		return err
	}

	// need_custom_fdiffs
	if err := w.writeBool(w.bestTemplate.NeedCustomFdiffs); err != nil {
		return err
	}

	// need_custom_chains
	if err := w.writeBool(w.bestTemplate.NeedCustomChains); err != nil {
		return err
	}

	// template_dependency_structure
	if w.descriptor.AttachedStructure != nil {
		if err := w.writeTemplateDependencyStructure(); err != nil {
			return err
		}
	}

	// active_decode_targets_bitmask
	if activeDecodeTargetsPresentFlag {
		if err := w.writeBits(uint64(*w.descriptor.ActiveDecodeTargetsBitmask), w.structure.NumDecodeTargets); err != nil {
			return err
		}
	}

	return nil
}

func (w *DependencyDescriptorWriter) writeTemplateDependencyStructure() error {
	if !(w.structure.StructureId >= 0 && w.structure.StructureId < MaxTemplates &&
		w.structure.NumDecodeTargets > 0 && w.structure.NumDecodeTargets <= MaxDecodeTargets) {
		return fmt.Errorf("invalid arguments, structureId: %d, numDecodeTargets: %d", w.structure.StructureId, w.structure.NumDecodeTargets)
	}

	if err := w.writeBits(uint64(w.structure.StructureId), 6); err != nil {
		return err
	}

	if err := w.writeBits(uint64(w.structure.NumDecodeTargets-1), 5); err != nil {
		return err
	}

	if err := w.writeTemplateLayers(); err != nil {
		return err
	}

	if err := w.writeTemplateDtis(); err != nil {
		return err
	}

	if err := w.writeTemplateFdiffs(); err != nil {
		return err
	}

	if err := w.writeTemplateChains(); err != nil {
		return err
	}

	hasResolutions := len(w.structure.Resolutions) > 0
	if err := w.writeBool(hasResolutions); err != nil {
		return err
	}
	return w.writeResolutions()
}

func (w *DependencyDescriptorWriter) writeTemplateLayers() error {
	if !(len(w.structure.Templates) > 0 && len(w.structure.Templates) <= MaxTemplates &&
		w.structure.Templates[0].SpatialId == 0 && w.structure.Templates[0].TemporalId == 0) {
		return fmt.Errorf("invalid templates, len %d, templates[0]: spatialId %d, temporalId %d", len(w.structure.Templates), w.structure.Templates[0].SpatialId, w.structure.Templates[0].TemporalId)
	}
	for i := 1; i < len(w.structure.Templates); i++ {
		nextLayerIdc := getNextLayerIdc(w.structure.Templates[i-1], w.structure.Templates[i])
		if nextLayerIdc >= 3 {
			return fmt.Errorf("invalid next_layer_idc %d", nextLayerIdc)
		}
		if err := w.writeBits(uint64(nextLayerIdc), 2); err != nil {
			return err
		}
	}
	return w.writeBits(uint64(noMoreLayer), 2)
}

func getNextLayerIdc(prevTemplate, nextTemplate *FrameDependencyTemplate) nextLayerIdcType {
	if nextTemplate.SpatialId == prevTemplate.SpatialId && nextTemplate.TemporalId == prevTemplate.TemporalId {
		return sameLayer
	} else if nextTemplate.SpatialId == prevTemplate.SpatialId && nextTemplate.TemporalId == prevTemplate.TemporalId+1 {
		return nextTemporalLayer
	} else if nextTemplate.SpatialId == prevTemplate.SpatialId+1 && nextTemplate.TemporalId == 0 {
		return nextSpatialLayer
	}

	return invalidLayer
}

func (w *DependencyDescriptorWriter) writeTemplateDtis() error {
	for _, t := range w.structure.Templates {
		for _, dti := range t.DecodeTargetIndications {
			if err := w.writeBits(uint64(dti), 2); err != nil {
				return err
			}
		}
	}

	return nil
}

func (w *DependencyDescriptorWriter) writeTemplateFdiffs() error {
	for _, t := range w.structure.Templates {
		for _, fdiff := range t.FrameDiffs {
			if err := w.writeBits(uint64(1<<4)|uint64(fdiff-1), 1+4); err != nil {
				return err
			}
		}
		// no more fdiffs for this template
		if err := w.writeBits(uint64(0), 1); err != nil {
			return err
		}
	}

	return nil
}

func (w *DependencyDescriptorWriter) writeTemplateChains() error {
	if err := w.writeNonSymmetric(uint32(w.structure.NumChains), uint32(w.structure.NumDecodeTargets+1)); err != nil {
		return err
	}

	if w.structure.NumChains == 0 {
		return nil
	}

	for _, protectedBy := range w.structure.DecodeTargetProtectedByChain {
		if err := w.writeNonSymmetric(uint32(protectedBy), uint32(w.structure.NumChains)); err != nil {
			return err
		}
	}

	for _, t := range w.structure.Templates {
		for _, chainDiff := range t.ChainDiffs {
			if err := w.writeBits(uint64(chainDiff), 4); err != nil {
				return err
			}
		}
	}
	return nil
}

func (w *DependencyDescriptorWriter) writeNonSymmetric(value, numValues uint32) error {
	return w.writer.WriteNonSymmetric(value, numValues)
}

func (w *DependencyDescriptorWriter) writeResolutions() error {
	for _, res := range w.structure.Resolutions {
		if err := w.writeBits(uint64(res.Width)-1, 16); err != nil {
			return err
		}
		if err := w.writeBits(uint64(res.Height)-1, 16); err != nil {
			return err
		}
	}
	return nil
}

func (w *DependencyDescriptorWriter) writeFrameDependencyDefinition() error {
	if w.bestTemplate.NeedCustomDtis {
		if err := w.writeFrameDtis(); err != nil {
			return err
		}
	}

	if w.bestTemplate.NeedCustomFdiffs {
		if err := w.writeFrameFdiffs(); err != nil {
			return err
		}
	}

	if w.bestTemplate.NeedCustomChains {
		if err := w.writeFrameChains(); err != nil {
			return err
		}
	}

	return nil
}

func (w *DependencyDescriptorWriter) writeFrameDtis() error {
	for _, dti := range w.descriptor.FrameDependencies.DecodeTargetIndications {
		if err := w.writeBits(uint64(dti), 2); err != nil {
			return err
		}
	}
	return nil
}

func (w *DependencyDescriptorWriter) writeFrameFdiffs() error {
	for _, fdiff := range w.descriptor.FrameDependencies.FrameDiffs {
		if fdiff <= (1 << 4) {
			if err := w.writeBits(uint64(1<<4)|uint64(fdiff-1), 2+4); err != nil {
				return err
			}
		} else if fdiff <= (1 << 8) {
			if err := w.writeBits(uint64(2<<8)|uint64(fdiff-1), 2+8); err != nil {
				return err
			}
		} else { // fdiff <= (1<<12)
			if err := w.writeBits(uint64(3<<12)|uint64(fdiff-1), 2+12); err != nil {
				return err
			}
		}
	}
	// no more fdiffs
	return w.writeBits(uint64(0), 2)
}

func (w *DependencyDescriptorWriter) writeFrameChains() error {
	for i := 0; i < w.structure.NumChains; i++ {
		chainDiff := 0
		if w.activeChains&(1<<i) != 0 {
			chainDiff = w.descriptor.FrameDependencies.ChainDiffs[i]
		}
		if err := w.writeBits(uint64(chainDiff), 8); err != nil {
			return err
		}
	}
	return nil
}

const mandatoryFieldSize = 1 + 1 + 6 + 16

func (w *DependencyDescriptorWriter) ValueSizeBits() int {
	valueSizeBits := mandatoryFieldSize + w.bestTemplate.ExtraSizeBits
	if w.hasExtendedFields() {
		valueSizeBits += 5
		if w.descriptor.AttachedStructure != nil {
			valueSizeBits += w.structureSizeBits()
		}
		if w.shouldWriteActiveDecodeTargetsBitmask() {
			valueSizeBits += w.structure.NumDecodeTargets
		}
	}

	return valueSizeBits
}

func (w *DependencyDescriptorWriter) shouldWriteActiveDecodeTargetsBitmask() bool {
	if w.descriptor.ActiveDecodeTargetsBitmask == nil {
		return false
	}

	allDecodeTargetsBitmask := (uint64(1) << w.structure.NumDecodeTargets) - 1
	if w.descriptor.AttachedStructure != nil && uint64(*w.descriptor.ActiveDecodeTargetsBitmask) == allDecodeTargetsBitmask {
		return false
	}

	return true
}

func (w *DependencyDescriptorWriter) structureSizeBits() int {
	// template_id offset (6 bits) and number of decode targets (5 bits)
	bits := 11
	// template layers
	bits += 2 * len(w.structure.Templates)
	// dtis
	bits += 2 * len(w.structure.Templates) * w.structure.NumDecodeTargets
	// fdiffs. each templates uses 1 + 5 * sizeof(fdiff) bits.
	bits += len(w.structure.Templates)
	for _, t := range w.structure.Templates {
		bits += 5 * len(t.FrameDiffs)
	}
	bits += SizeNonSymmetricBits(uint32(w.structure.NumChains), uint32(w.structure.NumDecodeTargets+1))
	if w.structure.NumChains > 0 {
		for _, protectedBy := range w.structure.DecodeTargetProtectedByChain {
			bits += SizeNonSymmetricBits(uint32(protectedBy), uint32(w.structure.NumChains))
		}
		bits += 4 * len(w.structure.Templates) * w.structure.NumChains
	}

	// resolutions
	bits += 1 + 32*len(w.structure.Resolutions)

	return bits
}
