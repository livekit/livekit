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

package videolayerselector

import (
	dd "github.com/livekit/livekit-server/pkg/sfu/rtpextension/dependencydescriptor"
	"github.com/livekit/protocol/logger"
)

type FrameChain struct {
	logger         logger.Logger
	decisions      *SelectorDecisionCache
	broken         bool
	chainIdx       int
	active         bool
	updatingActive bool

	expectFrames []uint64
}

func NewFrameChain(decisions *SelectorDecisionCache, chainIdx int, logger logger.Logger) *FrameChain {
	return &FrameChain{
		logger:    logger,
		decisions: decisions,
		broken:    true,
		chainIdx:  chainIdx,
		active:    false,
	}
}

func (fc *FrameChain) OnFrame(extFrameNum uint64, fd *dd.FrameDependencyTemplate) bool {
	if !fc.active {
		return false
	}

	if len(fd.ChainDiffs) <= fc.chainIdx {
		fc.logger.Warnw("invalid frame chain diff", nil, "chanIdx", fc.chainIdx, "frame", extFrameNum, "fd", fd)
		return fc.broken
	}

	// A decodable frame with frame_chain_fdiff equal to 0 indicates that the Chain is intact.
	if fd.ChainDiffs[fc.chainIdx] == 0 {
		if fc.broken {
			fc.broken = false
			// fc.logger.Debugw("frame chain intact", "chanIdx", fc.chainIdx, "frame", extFrameNum)
		}
		fc.expectFrames = fc.expectFrames[:0]
		return true
	}

	if fc.broken {
		return false
	}

	prevFrameInChain := extFrameNum - uint64(fd.ChainDiffs[fc.chainIdx])
	sd, err := fc.decisions.GetDecision(prevFrameInChain)
	if err != nil {
		fc.logger.Debugw("could not get decision", "err", err, "chanIdx", fc.chainIdx, "frame", extFrameNum, "prevFrame", prevFrameInChain)
	}

	var intact bool
	switch {
	case sd == selectorDecisionForwarded:
		intact = true

	case sd == selectorDecisionUnknown:
		// If the previous frame is unknown, means it has not arrived but could be recovered by NACK / out-of-order arrival,
		// set up a expected callback here to determine if the chain is broken or intact
		if fc.decisions.ExpectDecision(prevFrameInChain, fc.OnExpectFrameChanged) {
			intact = true
			fc.expectFrames = append(fc.expectFrames, prevFrameInChain)
		}
	}

	if !intact {
		fc.broken = true
		// fc.logger.Debugw("frame chain broken", "chanIdx", fc.chainIdx, "sd", sd, "frame", extFrameNum, "prevFrame", prevFrameInChain)
	}
	return intact
}

func (fc *FrameChain) OnExpectFrameChanged(frameNum uint64, decision selectorDecision) {
	if fc.broken {
		return
	}

	for i, f := range fc.expectFrames {
		if f == frameNum {
			if decision != selectorDecisionForwarded {
				fc.broken = true
				// fc.logger.Debugw("frame chain broken", "chanIdx", fc.chainIdx, "sd", decision, "frame", frameNum)
			}
			fc.expectFrames[i] = fc.expectFrames[len(fc.expectFrames)-1]
			fc.expectFrames = fc.expectFrames[:len(fc.expectFrames)-1]
			break
		}
	}
}

func (fc *FrameChain) Broken() bool {
	return fc.broken
}

func (fc *FrameChain) BeginUpdateActive() {
	fc.updatingActive = false
}

func (fc *FrameChain) UpdateActive(active bool) {
	fc.updatingActive = fc.updatingActive || active
}

func (fc *FrameChain) EndUpdateActive() {
	active := fc.updatingActive
	fc.updatingActive = false

	if active == fc.active {
		return
	}

	// if the chain transit from inactive to active, reset broken to wait a decodable SWITCH frame
	if !fc.active {
		fc.broken = true
		fc.logger.Debugw("frame chain broken by inactive", "chanIdx", fc.chainIdx)
	}

	fc.active = active
}
