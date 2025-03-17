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
	"go.uber.org/zap/zapcore"
)

const (
	defaultRtt           = 70
	ignoreRetransmission = 100 // Ignore packet retransmission after ignoreRetransmission milliseconds
	maxAck               = 3
)

type packetMeta struct {
	// Original extended sequence number from stream.
	// The original extended sequence number is used to find the original
	// packet from publisher
	sourceSeqNo uint64
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

func (pm packetMeta) MarshalLogObject(e zapcore.ObjectEncoder) error {
	e.AddUint64("sourceSeqNo", pm.sourceSeqNo)
	e.AddUint16("targetSeqNo", pm.targetSeqNo)
	e.AddInt8("layer", pm.layer)
	e.AddUint8("nacked", pm.nacked)
	e.AddUint8("numCodecBytesIn", pm.numCodecBytesIn)
	if len(pm.codecBytesSlice) != 0 {
		e.AddInt("codecBytesSlice", len(pm.codecBytesSlice))
	} else {
		e.AddUint8("numCodecBytesOut", pm.numCodecBytesOut)
	}
	if len(pm.ddBytesSlice) != 0 {
		e.AddInt("ddBytesSlice", len(pm.ddBytesSlice))
	} else {
		e.AddUint8("ddBytesSize", pm.ddBytesSize)
	}
	if len(pm.actBytes) != 0 {
		e.AddInt("actBytes", len(pm.actBytes))
	}
	return nil
}

type extPacketMeta struct {
	packetMeta
	extSequenceNumber uint64
	extTimestamp      uint64
}

func (epm extPacketMeta) MarshalLogObject(e zapcore.ObjectEncoder) error {
	e.AddObject("packetMeta", epm.packetMeta)
	e.AddUint64("extSequenceNumber", epm.extSequenceNumber)
	return nil
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
	if size == 0 {
		return nil
	}

	s := &sequencer{
		size:      size,
		startTime: time.Now().UnixNano(),
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
	packetTime int64,
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
		sourceSeqNo:     extIncomingSN,
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
		for esn := extStartSNInclusive; esn != extEndSNInclusive+1; esn++ {
			diff := int64(esn - s.extHighestSN)
			if diff >= 0 || diff < -int64(s.size) {
				// too old OR too new (too new should not happen, just be safe)
				continue
			}

			snOffset, err := s.snRangeMap.GetValue(esn)
			if err != nil {
				s.logger.Errorw("could not get sequence number offset", err, "sn", esn)
				continue
			}

			slot := (esn - snOffset) % uint64(s.size)
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
	refTime := s.getRefTime(time.Now().UnixNano())
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

func (s *sequencer) lookupExtPacketMeta(extSN uint64) *extPacketMeta {
	s.Lock()
	defer s.Unlock()

	if !s.initialized {
		return nil
	}

	snOffset := uint64(0)
	var err error
	if s.snRangeMap != nil {
		snOffset, err = s.snRangeMap.GetValue(extSN)
		if err != nil {
			return nil
		}
	}

	extSNAdjusted := extSN - snOffset
	extHighestSNAdjusted := s.extHighestSN - s.snOffset
	if extHighestSNAdjusted-extSNAdjusted >= uint64(s.size) {
		// too old
		return nil
	}

	slot := extSNAdjusted % uint64(s.size)
	meta := &s.meta[slot]
	if s.isInvalidSlot(int(slot)) {
		// invalid slot access could happen if padding packets exclusion range could not be recorded
		return nil
	}

	extTS := uint64(meta.timestamp) + (s.extHighestTS & 0xFFFF_FFFF_0000_0000)
	if meta.timestamp > uint32(s.extHighestTS) {
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
	return &epm
}

func (s *sequencer) getRefTime(at int64) uint32 {
	return uint32((at - s.startTime) / 1e6)
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
