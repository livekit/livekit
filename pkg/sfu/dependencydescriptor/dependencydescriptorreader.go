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
	"errors"
)

var (
	ErrDDReaderNoStructure              = errors.New("DependencyDescriptorReader: Structure is nil")
	ErrDDReaderTemplateWithoutStructure = errors.New("DependencyDescriptorReader: has templateDependencyStructurePresentFlag but AttachedStructure is nil")
	ErrDDReaderTooManyTemplates         = errors.New("DependencyDescriptorReader: too many templates")
	ErrDDReaderTooManyTemporalLayers    = errors.New("DependencyDescriptorReader: too many temporal layers")
	ErrDDReaderTooManySpatialLayers     = errors.New("DependencyDescriptorReader: too many spatial layers")
	ErrDDReaderInvalidTemplateIndex     = errors.New("DependencyDescriptorReader: invalid template index")
	ErrDDReaderInvalidSpatialLayer      = errors.New("DependencyDescriptorReader: invalid spatial layer, should be less than the number of resolutions")
	ErrDDReaderNumDTIMismatch           = errors.New("DependencyDescriptorReader: decode target indications length mismatch with structure num decode targets")
	ErrDDReaderNumChainDiffsMismatch    = errors.New("DependencyDescriptorReader: chain diffs length mismatch with structure num chains")
)

type DependencyDescriptorReader struct {
	// Output.
	descriptor *DependencyDescriptor

	// Values that are needed while reading the descriptor, but can be discarded
	// when reading is complete.
	buffer                         *BitStreamReader
	frameDependencyTemplateId      int
	activeDecodeTargetsPresentFlag bool
	customDtisFlag                 bool
	customFdiffsFlag               bool
	customChainsFlag               bool
	structure                      *FrameDependencyStructure
}

func NewDependencyDescriptorReader(buf []byte, structure *FrameDependencyStructure, descriptor *DependencyDescriptor) *DependencyDescriptorReader {
	buffer := NewBitStreamReader(buf)
	return &DependencyDescriptorReader{
		buffer:     buffer,
		descriptor: descriptor,
		structure:  structure,
	}
}

func (r *DependencyDescriptorReader) Parse() (int, error) {
	if err := r.readMandatoryFields(); err != nil {
		return 0, err
	}
	if len(r.buffer.buf) > 3 {
		err := r.readExtendedFields()
		if err != nil {
			return 0, err
		}
	}

	if r.descriptor.AttachedStructure != nil {
		r.structure = r.descriptor.AttachedStructure
	}

	if r.structure == nil {
		r.buffer.Invalidate()
		return 0, ErrDDReaderNoStructure
	}

	if r.activeDecodeTargetsPresentFlag {
		bitmask, err := r.buffer.ReadBits(r.structure.NumDecodeTargets)
		if err != nil {
			return 0, err
		}
		mask := uint32(bitmask)
		r.descriptor.ActiveDecodeTargetsBitmask = &mask
	}

	err := r.readFrameDependencyDefinition()
	if err != nil {
		return 0, err
	}
	return r.buffer.BytesRead(), nil
}

func (r *DependencyDescriptorReader) readMandatoryFields() error {
	var err error
	r.descriptor.FirstPacketInFrame, err = r.buffer.ReadBool()
	if err != nil {
		return err
	}

	r.descriptor.LastPacketInFrame, err = r.buffer.ReadBool()
	if err != nil {
		return err
	}

	templateID, err := r.buffer.ReadBits(6)
	if err != nil {
		return err
	}
	r.frameDependencyTemplateId = int(templateID)

	frameNumber, err := r.buffer.ReadBits(16)
	if err != nil {
		return err
	}
	r.descriptor.FrameNumber = uint16(frameNumber)
	return nil
}

func (r *DependencyDescriptorReader) readExtendedFields() error {
	templateDependencyStructurePresentFlag, err := r.buffer.ReadBool()
	if err != nil {
		return err
	}

	flag, err := r.buffer.ReadBool()
	if err != nil {
		return err
	}
	r.activeDecodeTargetsPresentFlag = flag

	flag, err = r.buffer.ReadBool()
	if err != nil {
		return err
	}
	r.customDtisFlag = flag

	flag, err = r.buffer.ReadBool()
	if err != nil {
		return err
	}
	r.customFdiffsFlag = flag

	flag, err = r.buffer.ReadBool()
	if err != nil {
		return err
	}
	r.customChainsFlag = flag

	if templateDependencyStructurePresentFlag {
		err = r.readTemplateDependencyStructure()
		if err != nil {
			return err
		}
		if r.descriptor.AttachedStructure == nil {
			return ErrDDReaderTemplateWithoutStructure
		}
		bitmask := uint32((uint64(1) << r.descriptor.AttachedStructure.NumDecodeTargets) - 1)
		r.descriptor.ActiveDecodeTargetsBitmask = &bitmask
	}
	return nil
}

func (r *DependencyDescriptorReader) readTemplateDependencyStructure() error {
	r.descriptor.AttachedStructure = &FrameDependencyStructure{}
	structureId, err := r.buffer.ReadBits(6)
	if err != nil {
		return err
	}
	r.descriptor.AttachedStructure.StructureId = int(structureId)

	numDecodeTargets, err := r.buffer.ReadBits(5)
	if err != nil {
		return err
	}
	r.descriptor.AttachedStructure.NumDecodeTargets = int(numDecodeTargets) + 1

	if err = r.readTemplateLayers(); err != nil {
		return err
	}
	if err = r.readTemplateDtis(); err != nil {
		return err
	}
	if err = r.readTemplateFdiffs(); err != nil {
		return err
	}
	if err = r.readTemplateChains(); err != nil {
		return err
	}

	flag, err := r.buffer.ReadBool()
	if err != nil {
		return err
	}
	if flag {
		return r.readResolutions()
	}
	return nil
}

type nextLayerIdcType int

const (
	sameLayer nextLayerIdcType = iota
	nextTemporalLayer
	nextSpatialLayer
	noMoreLayer
	invalidLayer
)

func (r *DependencyDescriptorReader) readTemplateLayers() error {
	var (
		templates             []*FrameDependencyTemplate
		temporalId, spatialId int
		nextLayerIdc          nextLayerIdcType
	)
	for {
		if len(templates) == MaxTemplates {
			return ErrDDReaderTooManyTemplates
		}

		var lastTemplate FrameDependencyTemplate
		templates = append(templates, &lastTemplate)
		lastTemplate.TemporalId = temporalId
		lastTemplate.SpatialId = spatialId

		idc, err := r.buffer.ReadBits(2)
		if err != nil {
			return err
		}
		nextLayerIdc = nextLayerIdcType(idc)

		if nextLayerIdc == nextTemporalLayer {
			temporalId++
			if temporalId >= MaxTemporalIds {
				return ErrDDReaderTooManyTemporalLayers
			}
		} else if nextLayerIdc == nextSpatialLayer {
			spatialId++
			temporalId = 0
			if spatialId >= MaxSpatialIds {
				return ErrDDReaderTooManySpatialLayers
			}
		}

		if !(nextLayerIdc != noMoreLayer && r.buffer.Ok()) {
			break
		}
	}

	r.descriptor.AttachedStructure.Templates = templates
	return nil
}

func (r *DependencyDescriptorReader) readTemplateDtis() error {
	structure := r.descriptor.AttachedStructure
	for _, template := range structure.Templates {
		if len(template.DecodeTargetIndications) < structure.NumDecodeTargets {
			template.DecodeTargetIndications = append(template.DecodeTargetIndications, make([]DecodeTargetIndication, structure.NumDecodeTargets-len(template.DecodeTargetIndications))...)
		} else {
			template.DecodeTargetIndications = template.DecodeTargetIndications[:structure.NumDecodeTargets]
		}

		for i := range template.DecodeTargetIndications {
			indication, err := r.buffer.ReadBits(2)
			if err != nil {
				return err
			}
			template.DecodeTargetIndications[i] = DecodeTargetIndication(indication)
		}
	}
	return nil
}

func (r *DependencyDescriptorReader) readTemplateFdiffs() error {
	for _, template := range r.descriptor.AttachedStructure.Templates {
		for {
			fdiffFollow, err := r.buffer.ReadBool()
			if err != nil {
				return err
			}
			if !fdiffFollow {
				break
			}
			fDiffMinusOne, err := r.buffer.ReadBits(4)
			if err != nil {
				return err
			}
			template.FrameDiffs = append(template.FrameDiffs, int(fDiffMinusOne+1))
		}
	}

	return nil
}

func (r *DependencyDescriptorReader) readTemplateChains() error {
	structure := r.descriptor.AttachedStructure

	numChains, err := r.buffer.ReadNonSymmetric(uint32(structure.NumDecodeTargets) + 1)
	if err != nil {
		return err
	}
	structure.NumChains = int(numChains)
	if structure.NumChains == 0 {
		return nil
	}

	for i := 0; i < structure.NumDecodeTargets; i++ {
		protectedByChain, err := r.buffer.ReadNonSymmetric(uint32(structure.NumChains))
		if err != nil {
			return err
		}
		structure.DecodeTargetProtectedByChain = append(structure.DecodeTargetProtectedByChain, int(protectedByChain))
	}

	for _, frameTemplate := range structure.Templates {
		for chainId := 0; chainId < structure.NumChains; chainId++ {
			chainDiff, err := r.buffer.ReadBits(4)
			if err != nil {
				return err
			}
			frameTemplate.ChainDiffs = append(frameTemplate.ChainDiffs, int(chainDiff))
		}
	}

	return nil
}

func (r *DependencyDescriptorReader) readResolutions() error {
	structure := r.descriptor.AttachedStructure
	// The way templates are bitpacked, they are always ordered by spatial_id.
	spatialLayers := structure.Templates[len(structure.Templates)-1].SpatialId + 1
	for sid := 0; sid < spatialLayers; sid++ {
		widthMinus1, err := r.buffer.ReadBits(16)
		if err != nil {
			return err
		}
		heightMinus1, err := r.buffer.ReadBits(16)
		if err != nil {
			return err
		}
		structure.Resolutions = append(structure.Resolutions, RenderResolution{
			Width:  int(widthMinus1 + 1),
			Height: int(heightMinus1 + 1),
		})
	}

	return nil
}

func (r *DependencyDescriptorReader) readFrameDependencyDefinition() error {
	templateIndex := (r.frameDependencyTemplateId + MaxTemplates - r.structure.StructureId) % MaxTemplates

	if templateIndex >= len(r.structure.Templates) {
		r.buffer.Invalidate()
		return ErrDDReaderInvalidTemplateIndex
	}

	// Copy all the fields from the matching template
	r.descriptor.FrameDependencies = r.structure.Templates[templateIndex].Clone()

	if r.customDtisFlag {
		err := r.readFrameDtis()
		if err != nil {
			return err
		}
	}

	if r.customFdiffsFlag {
		err := r.readFrameFdiffs()
		if err != nil {
			return err
		}
	}

	if r.customChainsFlag {
		err := r.readFrameChains()
		if err != nil {
			return err
		}
	}

	if len(r.structure.Resolutions) == 0 {
		r.descriptor.Resolution = nil
	} else {
		// Format guarantees that if there were resolutions in the last structure,
		// then each spatial layer got one.
		if r.descriptor.FrameDependencies.SpatialId >= len(r.structure.Resolutions) {
			r.buffer.Invalidate()
			return ErrDDReaderInvalidSpatialLayer
		}
		res := r.structure.Resolutions[r.descriptor.FrameDependencies.SpatialId]
		r.descriptor.Resolution = &res
	}

	return nil
}

func (r *DependencyDescriptorReader) readFrameDtis() error {
	if len(r.descriptor.FrameDependencies.DecodeTargetIndications) != r.structure.NumDecodeTargets {
		return ErrDDReaderNumDTIMismatch
	}

	for i := range r.descriptor.FrameDependencies.DecodeTargetIndications {
		indication, err := r.buffer.ReadBits(2)
		if err != nil {
			return err
		}
		r.descriptor.FrameDependencies.DecodeTargetIndications[i] = DecodeTargetIndication(indication)
	}
	return nil
}

func (r *DependencyDescriptorReader) readFrameFdiffs() error {
	r.descriptor.FrameDependencies.FrameDiffs = r.descriptor.FrameDependencies.FrameDiffs[:0]
	for {
		nexFdiffSize, err := r.buffer.ReadBits(2)
		if err != nil {
			return err
		}
		if nexFdiffSize == 0 {
			break
		}
		fDiffMinusOne, err := r.buffer.ReadBits(int(nexFdiffSize * 4))
		if err != nil {
			return err
		}
		r.descriptor.FrameDependencies.FrameDiffs = append(r.descriptor.FrameDependencies.FrameDiffs, int(fDiffMinusOne+1))
	}

	return nil
}

func (r *DependencyDescriptorReader) readFrameChains() error {
	if len(r.descriptor.FrameDependencies.ChainDiffs) != r.structure.NumChains {
		return ErrDDReaderNumChainDiffsMismatch
	}

	for i := range r.descriptor.FrameDependencies.ChainDiffs {
		chainDiff, err := r.buffer.ReadBits(8)
		if err != nil {
			return err
		}
		r.descriptor.FrameDependencies.ChainDiffs[i] = int(chainDiff)
	}
	return nil
}
