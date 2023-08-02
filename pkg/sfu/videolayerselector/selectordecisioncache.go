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
	"fmt"
)

// ----------------------------------------------------------------------

type selectorDecision int

const (
	selectorDecisionMissing selectorDecision = iota
	selectorDecisionDropped
	selectorDecisionForwarded
	selectorDecisionUnknown
)

func (s selectorDecision) String() string {
	switch s {
	case selectorDecisionMissing:
		return "MISSING"
	case selectorDecisionDropped:
		return "DROPPED"
	case selectorDecisionForwarded:
		return "FORWARDED"
	case selectorDecisionUnknown:
		return "UNKNOWN"
	default:
		return fmt.Sprintf("%d", int(s))
	}
}

// ----------------------------------------------------------------------

type SelectorDecisionCache struct {
	initialized    bool
	base           uint64
	last           uint64
	masks          []uint64
	numEntries     uint64
	numNackEntries uint64

	onExpectEntityChanged map[uint64][]func(entity uint64, decision selectorDecision)
}

func NewSelectorDecisionCache(maxNumElements uint64, numNackEntries uint64) *SelectorDecisionCache {
	numElements := (maxNumElements*2 + 63) / 64
	return &SelectorDecisionCache{
		masks:                 make([]uint64, numElements),
		numEntries:            numElements * 32, // 2 bits per entry
		numNackEntries:        numNackEntries,
		onExpectEntityChanged: make(map[uint64][]func(entity uint64, decision selectorDecision)),
	}
}

func (s *SelectorDecisionCache) AddForwarded(entity uint64) {
	s.addEntity(entity, selectorDecisionForwarded)
}

func (s *SelectorDecisionCache) AddDropped(entity uint64) {
	s.addEntity(entity, selectorDecisionDropped)
}

func (s *SelectorDecisionCache) GetDecision(entity uint64) (selectorDecision, error) {
	if !s.initialized || entity < s.base {
		return selectorDecisionMissing, nil
	}

	if entity > s.last {
		return selectorDecisionUnknown, nil
	}

	offset := s.last - entity
	if offset >= s.numEntries {
		// asking for something too old
		return selectorDecisionMissing, fmt.Errorf("too old, oldest: %d, asking: %d", s.last-s.numEntries+1, entity)
	}

	return s.getEntity(entity), nil
}

func (s *SelectorDecisionCache) ExpectDecision(entity uint64, f func(entity uint64, decision selectorDecision)) bool {
	if !s.initialized || entity < s.base {
		return false
	}

	if entity < s.last {
		offset := s.last - entity
		if offset >= s.numEntries {
			return false // too old
		}
	}

	s.onExpectEntityChanged[entity] = append(s.onExpectEntityChanged[entity], f)
	return true
}

func (s *SelectorDecisionCache) addEntity(entity uint64, sd selectorDecision) {
	if !s.initialized {
		s.initialized = true
		s.base = entity
		s.last = entity
		s.setEntity(entity, sd)
		return
	}

	if entity <= s.base {
		// before base, too old
		return
	}

	if entity <= s.last {
		s.setEntity(entity, sd)
		return
	}

	for e := s.last + 1; e != entity; e++ {
		s.setEntity(e, selectorDecisionUnknown)
	}

	// update [last+1-nack, entity-nack) to missing
	missingStart := s.last
	if missingStart > s.numNackEntries+s.base {
		missingStart -= s.numNackEntries
	} else {
		missingStart = s.base
	}
	missingEnd := entity
	if missingEnd > s.numNackEntries+s.base {
		missingEnd -= s.numNackEntries
	} else {
		missingEnd = s.base
	}
	if missingEnd > missingStart {
		for e := missingStart; e != missingEnd; e++ {
			s.setEntityIfUnknown(e, selectorDecisionMissing)
		}
	}

	s.setEntity(entity, sd)
	s.last = entity

	for e, fns := range s.onExpectEntityChanged {
		if e+s.numEntries < s.last {
			delete(s.onExpectEntityChanged, e)
			for _, f := range fns {
				f(e, selectorDecisionMissing)
			}
		}
	}
}

func (s *SelectorDecisionCache) setEntityIfUnknown(entity uint64, sd selectorDecision) {
	if s.getEntity(entity) == selectorDecisionUnknown {
		s.setEntity(entity, sd)
	}
}

func (s *SelectorDecisionCache) setEntity(entity uint64, sd selectorDecision) {
	index, bitpos := s.getPos(entity)
	s.masks[index] &= ^(0x3 << bitpos) // clear before bitwise OR
	s.masks[index] |= (uint64(sd) & 0x3) << bitpos

	if sd != selectorDecisionUnknown {
		if fns, ok := s.onExpectEntityChanged[entity]; ok {
			delete(s.onExpectEntityChanged, entity)
			for _, f := range fns {
				f(entity, sd)
			}
		}
	}
}

func (s *SelectorDecisionCache) getEntity(entity uint64) selectorDecision {
	index, bitpos := s.getPos(entity)
	return selectorDecision((s.masks[index] >> bitpos) & 0x3)
}

func (s *SelectorDecisionCache) getPos(entity uint64) (int, int) {
	// 2 bits per entity, a uint64 mask can hold 32 entities
	offset := (entity - s.base) % s.numEntries
	return int(offset >> 5), int(offset&0x1F) * 2
}
