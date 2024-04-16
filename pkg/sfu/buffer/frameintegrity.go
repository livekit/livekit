// Copyright 2024 LiveKit, Inc.
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
	dd "github.com/livekit/livekit-server/pkg/sfu/rtpextension/dependencydescriptor"
)

type FrameEntity struct {
	startSeq  *uint64
	endSeq    *uint64
	integrity bool

	pktHistory *PacketHistory
}

func (fe *FrameEntity) AddPacket(extSeq uint64, ddVal *dd.DependencyDescriptor) {
	// duplicate packet
	if fe.integrity {
		return
	}

	if fe.startSeq == nil && ddVal.FirstPacketInFrame {
		fe.startSeq = &extSeq
	}
	if fe.endSeq == nil && ddVal.LastPacketInFrame {
		fe.endSeq = &extSeq
	}

	if fe.startSeq != nil && fe.endSeq != nil {
		if fe.pktHistory.PacketsConsecutive(*fe.startSeq, *fe.endSeq) {
			fe.integrity = true
		}
	}
}

func (fe *FrameEntity) Reset() {
	fe.integrity = false
	fe.startSeq, fe.endSeq = nil, nil
}

func (fe *FrameEntity) Integrity() bool {
	return fe.integrity
}

// ------------------------------

type PacketHistory struct {
	base        uint64
	last        uint64
	bits        []uint64
	packetCount int
	inited      bool
}

func NewPacketHistory(packetCount int) *PacketHistory {
	packetCount = (packetCount + 63) / 64 * 64
	return &PacketHistory{
		bits:        make([]uint64, packetCount/64),
		packetCount: packetCount,
	}
}

func (ph *PacketHistory) AddPacket(extSeq uint64) {
	if !ph.inited {
		ph.inited = true
		ph.base = uint64(extSeq)
		// set base to extSeq-100 to avoid out-of-order packets belongs to first frame to be dropped
		if ph.base > 100 {
			ph.base -= 100
		} else {
			ph.base = 0
		}
		ph.last = uint64(extSeq)
		ph.set(extSeq, true)
		return
	}

	if extSeq <= ph.base {
		// too old
		return
	}

	if extSeq <= ph.last {
		if ph.last-extSeq < uint64(ph.packetCount) {
			ph.set(extSeq, true)
		}
		return
	}

	for i := ph.last + 1; i < extSeq; i++ {
		ph.set(i, false)
	}

	ph.set(extSeq, true)
	ph.last = extSeq
}

func (ph *PacketHistory) getPos(seq uint64) (index, offset int) {
	idx := (seq - ph.base) % uint64(ph.packetCount)
	return int(idx >> 6), int(idx % 64)
}

func (ph *PacketHistory) set(seq uint64, received bool) {
	idx, offset := ph.getPos(seq)
	if !received {
		ph.bits[idx] &= ^(1 << offset)
	} else {
		ph.bits[idx] |= 1 << (offset)
	}
}

func (ph *PacketHistory) PacketsConsecutive(start, end uint64) bool {
	if start > end {
		return false
	}

	if end-start >= uint64(ph.packetCount) {
		return false
	}

	startIndex, startOffset := ph.getPos(start)
	endIndex, endOffset := ph.getPos(end)

	if startIndex == endIndex && end-start <= 64 {
		testBits := uint64((1<<(endOffset-startOffset+1))-1) << startOffset
		return ph.bits[startIndex]&testBits == testBits
	}

	if (ph.bits[startIndex]>>(startOffset))+1 != 1<<(64-startOffset) {
		return false
	}

	for i := startIndex + 1; i != endIndex; i++ {
		if i == len(ph.bits) {
			i = 0
			if i == endIndex {
				break
			}
		}
		if ph.bits[i]+1 != 0 {
			return false
		}
	}

	testBits := uint64((1 << (endOffset + 1)) - 1)
	return ph.bits[endIndex]&testBits == testBits
}

// ------------------------------

type FrameIntegrityChecker struct {
	frameCount int
	frames     []FrameEntity
	base       uint64
	last       uint64

	pktHistory *PacketHistory
	inited     bool
}

func NewFrameIntegrityChecker(frameCount, packetCount int) *FrameIntegrityChecker {
	fc := &FrameIntegrityChecker{
		frames:     make([]FrameEntity, frameCount),
		pktHistory: NewPacketHistory(packetCount),
		frameCount: frameCount,
	}

	for i := range fc.frames {
		fc.frames[i].pktHistory = fc.pktHistory
		fc.frames[i].Reset()
	}
	return fc
}

func (fc *FrameIntegrityChecker) AddPacket(extSeq uint64, extFrameNum uint64, ddVal *dd.DependencyDescriptor) {
	fc.pktHistory.AddPacket(extSeq)

	if !fc.inited {
		fc.inited = true
		fc.base = extFrameNum
		fc.last = extFrameNum
	}

	if extFrameNum < fc.base {
		// frame too old
		return
	}

	if extFrameNum <= fc.last {
		if fc.last-extFrameNum >= uint64(fc.frameCount) {
			// frame too old
			return
		}
		fc.frames[int(extFrameNum-fc.base)%fc.frameCount].AddPacket(extSeq, ddVal)
		return
	}

	// reset missing frames
	for i := fc.last + 1; i <= extFrameNum; i++ {
		fc.frames[int(i-fc.base)%fc.frameCount].Reset()
	}
	fc.frames[int(extFrameNum-fc.base)%fc.frameCount].AddPacket(extSeq, ddVal)
	fc.last = extFrameNum
}

func (fc *FrameIntegrityChecker) FrameIntegrity(extFrameNum uint64) bool {
	if extFrameNum < fc.base || extFrameNum > fc.last || fc.last-extFrameNum >= uint64(fc.frameCount) {
		return false
	}

	return fc.frames[int(extFrameNum-fc.base)%fc.frameCount].Integrity()
}
