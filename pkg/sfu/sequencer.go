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

package sfu

import (
	"math"
	"sync"
	"time"

	"github.com/livekit/livekit-server/pkg/sfu/utils"
	"github.com/livekit/protocol/logger"
)

const (
	defaultRtt           = 70
	ignoreRetransmission = 100 // Ignore packet retransmission after ignoreRetransmission milliseconds
	maxAck               = 3
)

func btoi(b bool) int {
	if b {
		return 1
	}

	return 0
}

func itob(i int) bool {
	return i != 0
}

type packetMeta struct {
	// Original sequence number from stream.
	// The original sequence number is used to find the original
	// packet from publisher
	sourceSeqNo uint16
	// Modified sequence number after offset.
	// This sequence number is used for the associated
	// down track, is modified according the offsets, and
	// must not be shared
	targetSeqNo uint16
	// Modified timestamp for current associated
	// down track.
	timestamp uint32
	// Modified marker
	marker bool
	// The last time this packet was nack requested.
	// Sometimes clients request the same packet more than once, so keep
	// track of the requested packets helps to avoid writing multiple times
	// the same packet.
	// The resolution is 1 ms counting after the sequencer start time.
	lastNack uint32
	// number of NACKs this packet has received
	nacked uint8
	// Spatial layer of packet
	layer int8
	// Information that differs depending on the codec
	codecBytes       [8]byte
	numCodecBytesIn  uint8
	numCodecBytesOut uint8
	codecBytesSlice  []byte
	// Dependency Descriptor of packet
	ddBytes      [8]byte
	ddBytesSize  uint8
	ddBytesSlice []byte
	// abs-capture-time of packet
	actBytes []byte
}

type extPacketMeta struct {
	packetMeta
	extSequenceNumber uint64
	extTimestamp      uint64
}

// Sequencer stores the packet sequence received by the down track
type sequencer struct {
	sync.Mutex
	size         int
	startTime    int64
	initialized  bool
	extStartSN   uint64
	extHighestSN uint64
	snOffset     uint64
	extHighestTS uint64
	meta         []packetMeta
	snRangeMap   *utils.RangeMap[uint64, uint64]
	rtt          uint32
	logger       logger.Logger
}

func newSequencer(size int, maybeSparse bool, logger logger.Logger) *sequencer {
	s := &sequencer{
		size:      size,
		startTime: time.Now().UnixMilli(),
		meta:      make([]packetMeta, size),
		rtt:       defaultRtt,
		logger:    logger,
	}

	if maybeSparse {
		s.snRangeMap = utils.NewRangeMap[uint64, uint64]((size + 1) / 2) // assume run lengths of at least 2 in between padding bursts
	}
	return s
}

func (s *sequencer) setRTT(rtt uint32) {
	s.Lock()
	defer s.Unlock()

	if rtt == 0 {
		s.rtt = defaultRtt
	} else {
		s.rtt = rtt
	}
}

func (s *sequencer) push(
	packetTime time.Time,
	extIncomingSN, extModifiedSN uint64,
	extModifiedTS uint64,
	marker bool,
	layer int8,
	codecBytes []byte,
	numCodecBytesIn int,
	ddBytes []byte,
	actBytes []byte,
) {
	s.Lock()
	defer s.Unlock()

	if !s.initialized {
		s.initialized = true
		s.extStartSN = extModifiedSN
		s.extHighestSN = extModifiedSN
		s.extHighestTS = extModifiedTS
		s.updateSNOffset()
	}

	if extModifiedSN < s.extStartSN {
		// old packet, should not happen
		return
	}

	extHighestSNAdjusted := s.extHighestSN - s.snOffset
	extModifiedSNAdjusted := extModifiedSN - s.snOffset
	if extModifiedSN < s.extHighestSN {
		if s.snRangeMap != nil {
			snOffset, err := s.snRangeMap.GetValue(extModifiedSN)
			if err != nil {
				s.logger.Errorw(
					"could not get sequence number offset", err,
					"extStartSN", s.extStartSN,
					"extHighestSN", s.extHighestSN,
					"extIncomingSN", extIncomingSN,
					"extModifiedSN", extModifiedSN,
					"snOffset", s.snOffset,
				)
				return
			}

			extModifiedSNAdjusted = extModifiedSN - snOffset
		}
	}

	if int64(extModifiedSNAdjusted-extHighestSNAdjusted) <= -int64(s.size) {
		s.logger.Warnw(
			"old packet, cannot be sequenced", nil,
			"extHighestSN", s.extHighestSN,
			"extIncomingSN", extIncomingSN,
			"extModifiedSN", extModifiedSN,
		)
		return
	}

	// invalidate missing sequence numbers
	if extModifiedSNAdjusted > extHighestSNAdjusted {
		numInvalidated := 0
		for esn := extHighestSNAdjusted + 1; esn != extModifiedSNAdjusted; esn++ {
			s.invalidateSlot(int(esn % uint64(s.size)))
			numInvalidated++
			if numInvalidated >= s.size {
				break
			}
		}
	}

	slot := extModifiedSNAdjusted % uint64(s.size)
	s.meta[slot] = packetMeta{
		sourceSeqNo:     uint16(extIncomingSN),
		targetSeqNo:     uint16(extModifiedSN),
		timestamp:       uint32(extModifiedTS),
		marker:          marker,
		layer:           layer,
		numCodecBytesIn: uint8(numCodecBytesIn),
		lastNack:        s.getRefTime(packetTime), // delay retransmissions after the original transmission
	}
	pm := &s.meta[slot]

	pm.numCodecBytesOut = uint8(len(codecBytes))
	if len(codecBytes) > len(pm.codecBytes) {
		pm.codecBytesSlice = append([]byte{}, codecBytes...)
	} else {
		copy(pm.codecBytes[:pm.numCodecBytesOut], codecBytes)
	}

	pm.ddBytesSize = uint8(len(ddBytes))
	if len(ddBytes) > len(pm.ddBytes) {
		pm.ddBytesSlice = append([]byte{}, ddBytes...)
	} else {
		copy(pm.ddBytes[:pm.ddBytesSize], ddBytes)
	}

	pm.actBytes = append([]byte{}, actBytes...)

	if extModifiedSN > s.extHighestSN {
		s.extHighestSN = extModifiedSN
	}
	if extModifiedTS > s.extHighestTS {
		s.extHighestTS = extModifiedTS
	}
}

func (s *sequencer) pushPadding(extStartSNInclusive uint64, extEndSNInclusive uint64) {
	s.Lock()
	defer s.Unlock()

	if s.snRangeMap == nil || !s.initialized {
		return
	}

	if extStartSNInclusive <= s.extHighestSN {
		// a higher sequence number has already been recorded with an offset,
		// adding an exclusion range before the highest means the offset of sequence numbers
		// after the exclusion range will be affected and all those higher sequence numbers
		// need to be patched.
		//
		// Not recording exclusion range means a few slots (of the size of exclusion range)
		// are wasted in this cycle. That should be fine as the exclusion ranges should be
		// a few packets at a time.
		if extEndSNInclusive >= s.extHighestSN {
			s.logger.Errorw("cannot exclude overlapping range", nil, "extHighestSN", s.extHighestSN, "startSN", extStartSNInclusive, "endSN", extEndSNInclusive)
		} else {
			s.logger.Warnw("cannot exclude old range", nil, "extHighestSN", s.extHighestSN, "startSN", extStartSNInclusive, "endSN", extEndSNInclusive)
		}

		// if exclusion range is before what has already been sequenced, invalidate exclusion range slots
		for sn := extStartSNInclusive; sn != extEndSNInclusive+1; sn++ {
			diff := int64(sn - s.extHighestSN)
			if diff >= 0 || diff < -int64(s.size) {
				// too old OR too new (too new should not happen, just be safe)
				continue
			}

			snOffset, err := s.snRangeMap.GetValue(sn)
			if err != nil {
				s.logger.Errorw("could not get sequence number offset", err, "sn", sn)
				continue
			}

			slot := (sn - snOffset) % uint64(s.size)
			s.invalidateSlot(int(slot))
		}
		return
	}

	if err := s.snRangeMap.ExcludeRange(extStartSNInclusive, extEndSNInclusive+1); err != nil {
		s.logger.Errorw("could not exclude range", err, "startSN", extStartSNInclusive, "endSN", extEndSNInclusive)
		return
	}

	s.extHighestSN = extEndSNInclusive
	s.updateSNOffset()
}

func (s *sequencer) getExtPacketMetas(seqNo []uint16) []extPacketMeta {
	s.Lock()
	defer s.Unlock()

	if !s.initialized {
		return nil
	}

	snOffset := uint64(0)
	var err error
	extPacketMetas := make([]extPacketMeta, 0, len(seqNo))
	refTime := s.getRefTime(time.Now())
	highestSN := uint16(s.extHighestSN)
	highestTS := uint32(s.extHighestTS)
	for _, sn := range seqNo {
		diff := highestSN - sn
		if diff > (1 << 15) {
			// out-of-order from head (should not happen, just be safe)
			continue
		}

		// find slot by adjusting for padding only packets that were not recorded in sequencer
		extSN := uint64(sn) + (s.extHighestSN & 0xFFFF_FFFF_FFFF_0000)
		if sn > highestSN {
			extSN -= (1 << 16)
		}

		if s.snRangeMap != nil {
			snOffset, err = s.snRangeMap.GetValue(extSN)
			if err != nil {
				// could be padding packet which is excluded and will not have value
				continue
			}
		}

		extSNAdjusted := extSN - snOffset
		extHighestSNAdjusted := s.extHighestSN - s.snOffset
		if extHighestSNAdjusted-extSNAdjusted >= uint64(s.size) {
			// too old
			continue
		}

		slot := extSNAdjusted % uint64(s.size)
		meta := &s.meta[slot]
		if meta.targetSeqNo != sn || s.isInvalidSlot(int(slot)) {
			// invalid slot access could happen if padding packets exclusion range could not be recorded
			continue
		}

		if meta.nacked < maxAck && refTime-meta.lastNack > uint32(math.Min(float64(ignoreRetransmission), float64(2*s.rtt))) {
			meta.nacked++
			meta.lastNack = refTime

			extTS := uint64(meta.timestamp) + (s.extHighestTS & 0xFFFF_FFFF_0000_0000)
			if meta.timestamp > highestTS {
				extTS -= (1 << 32)
			}
			epm := extPacketMeta{
				packetMeta:        *meta,
				extSequenceNumber: extSN,
				extTimestamp:      extTS,
			}
			epm.codecBytesSlice = append([]byte{}, meta.codecBytesSlice...)
			epm.ddBytesSlice = append([]byte{}, meta.ddBytesSlice...)
			epm.actBytes = append([]byte{}, meta.actBytes...)
			extPacketMetas = append(extPacketMetas, epm)
		}
	}

	return extPacketMetas
}

func (s *sequencer) getRefTime(at time.Time) uint32 {
	return uint32(at.UnixMilli() - s.startTime)
}

func (s *sequencer) updateSNOffset() {
	if s.snRangeMap == nil {
		return
	}

	snOffset, err := s.snRangeMap.GetValue(s.extHighestSN + 1)
	if err != nil {
		s.logger.Errorw("could not update sequence number offset", err, "extHighestSN", s.extHighestSN)
		return
	}
	s.snOffset = snOffset
}

func (s *sequencer) invalidateSlot(slot int) {
	if slot >= len(s.meta) {
		return
	}

	s.meta[slot] = packetMeta{
		sourceSeqNo: 0,
		targetSeqNo: 0,
		lastNack:    0,
	}
}

func (s *sequencer) isInvalidSlot(slot int) bool {
	if slot >= len(s.meta) {
		return true
	}

	meta := &s.meta[slot]
	return meta.sourceSeqNo == 0 && meta.targetSeqNo == 0 && meta.lastNack == 0
}
