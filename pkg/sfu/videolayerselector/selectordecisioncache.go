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
	initialized bool
	base        uint64
	last        uint64
	masks       []uint64
	numEntries  uint64
}

func NewSelectorDecisionCache(maxNumElements uint64) *SelectorDecisionCache {
	numElements := (maxNumElements*2 + 63) / 64
	return &SelectorDecisionCache{
		masks:      make([]uint64, numElements),
		numEntries: numElements * 32, // 2 bits per entry
	}
}

func (s *SelectorDecisionCache) AddForwarded(entity uint64) {
	s.addEntity(entity, selectorDecisionForwarded)
}

func (s *SelectorDecisionCache) AddDropped(entity uint64) {
	s.addEntity(entity, selectorDecisionDropped)
}

func (s *SelectorDecisionCache) GetDecision(entity uint64) (selectorDecision, error) {
	if !s.initialized || entity > s.last || entity < s.base {
		return selectorDecisionUnknown, nil
	}

	offset := s.last - entity
	if offset >= s.numEntries {
		// asking for something too old
		return selectorDecisionUnknown, fmt.Errorf("too old, oldest: %d, asking: %d", s.last-s.numEntries+1, entity)
	}

	return s.getEntity(entity), nil
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
		s.setEntity(e, selectorDecisionMissing)
	}
	s.setEntity(entity, sd)
	s.last = entity
}

func (s *SelectorDecisionCache) setEntity(entity uint64, sd selectorDecision) {
	index, bitpos := s.getPos(entity)
	s.masks[index] &= ^(0x3 << bitpos) // clear before bitwise OR
	s.masks[index] |= (uint64(sd) & 0x3) << bitpos
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
