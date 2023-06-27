package videolayerselector

import (
	"fmt"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	dd "github.com/livekit/livekit-server/pkg/sfu/dependencydescriptor"
)

type DecodeTarget struct {
	buffer.DependencyDescriptorDecodeTarget
	chain  *FrameChain
	active bool
}

type FrameDetectionResult struct {
	TargetValid bool
	DTI         dd.DecodeTargetIndication
}

func NewDecodeTarget(target buffer.DependencyDescriptorDecodeTarget, chain *FrameChain) *DecodeTarget {
	return &DecodeTarget{
		DependencyDescriptorDecodeTarget: target,
		chain:                            chain,
	}
}

func (dt *DecodeTarget) Valid() bool {
	return dt.chain == nil || !dt.chain.Broken()
}

func (dt *DecodeTarget) Active() bool {
	return dt.active
}

func (dt *DecodeTarget) UpdateActive(activeBitmask uint32) {
	active := (activeBitmask & (1 << dt.Target)) != 0
	dt.active = active
	if dt.chain != nil {
		dt.chain.UpdateActive(active)
	}
}

func (dt *DecodeTarget) OnFrame(extFrameNum uint64, fd *dd.FrameDependencyTemplate) (FrameDetectionResult, error) {
	result := FrameDetectionResult{}
	if len(fd.DecodeTargetIndications) <= dt.Target {
		return result, fmt.Errorf("mismatch target %d and len(DecodeTargetIndications) %d", dt.Target, len(fd.DecodeTargetIndications))
	}

	result.DTI = fd.DecodeTargetIndications[dt.Target]
	// The encoder can choose not to use frame chain in theory, and we need to trace every required frame is decodable in this case.
	// But we don't observe this in browser and it makes no sense to not use the chain with svc, so only use chain to detect decode target broken now,
	// and always return decodable if it is not protect by chain.
	result.TargetValid = dt.Valid()
	return result, nil
}
