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
	"fmt"

	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/sfu/utils"
)

//
// RTPMunger
//
type SequenceNumberOrdering int

const (
	SequenceNumberOrderingContiguous SequenceNumberOrdering = iota
	SequenceNumberOrderingOutOfOrder
	SequenceNumberOrderingGap
	SequenceNumberOrderingDuplicate
)

const (
	RtxGateWindow = 2000

	SnOffsetCacheSize = 4096
	SnOffsetCacheMask = SnOffsetCacheSize - 1
)

type TranslationParamsRTP struct {
	snOrdering     SequenceNumberOrdering
	sequenceNumber uint16
	timestamp      uint32
}

type SnTs struct {
	sequenceNumber uint16
	timestamp      uint32
}

// ----------------------------------------------------------------------

type RTPMungerState struct {
	LastSN uint16
	LastTS uint32
}

func (r RTPMungerState) String() string {
	return fmt.Sprintf("RTPMungerState{lastSN: %d, lastTS: %d)", r.LastSN, r.LastTS)
}

// ----------------------------------------------------------------------

type RTPMunger struct {
	logger logger.Logger

	extHighestIncomingSN uint32
	snRangeMap           *utils.RangeMap[uint32, uint32]

	lastSN     uint16
	lastTS     uint32
	tsOffset   uint32
	lastMarker bool

	rtxGateSn         uint16
	isInRtxGateRegion bool
}

func NewRTPMunger(logger logger.Logger) *RTPMunger {
	return &RTPMunger{
		logger:     logger,
		snRangeMap: utils.NewRangeMap[uint32, uint32](100),
	}
}

func (r *RTPMunger) DebugInfo() map[string]interface{} {
	snOffset, _ := r.snRangeMap.GetValue(r.extHighestIncomingSN)
	return map[string]interface{}{
		"ExtHighestIncomingSN": r.extHighestIncomingSN,
		"LastSN":               r.lastSN,
		"SNOffset":             snOffset,
		"LastTS":               r.lastTS,
		"TSOffset":             r.tsOffset,
		"LastMarker":           r.lastMarker,
	}
}

func (r *RTPMunger) GetLast() RTPMungerState {
	return RTPMungerState{
		LastSN: r.lastSN,
		LastTS: r.lastTS,
	}
}

func (r *RTPMunger) SeedLast(state RTPMungerState) {
	r.lastSN = state.LastSN
	r.lastTS = state.LastTS
}

func (r *RTPMunger) SetLastSnTs(extPkt *buffer.ExtPacket) {
	r.extHighestIncomingSN = extPkt.ExtSequenceNumber - 1
	r.lastSN = extPkt.Packet.SequenceNumber
	r.lastTS = extPkt.Packet.Timestamp
}

func (r *RTPMunger) UpdateSnTsOffsets(extPkt *buffer.ExtPacket, snAdjust uint16, tsAdjust uint32) {
	r.extHighestIncomingSN = extPkt.ExtSequenceNumber - 1
	r.snRangeMap.ClearAndResetValue(uint32(extPkt.Packet.SequenceNumber - r.lastSN - snAdjust))
	r.tsOffset = extPkt.Packet.Timestamp - r.lastTS - tsAdjust
}

func (r *RTPMunger) PacketDropped(extPkt *buffer.ExtPacket) {
	if r.extHighestIncomingSN != extPkt.ExtSequenceNumber {
		return
	}

	r.snRangeMap.IncValue(1)

	snOffset, err := r.snRangeMap.GetValue(extPkt.ExtSequenceNumber)
	if err != nil {
		r.logger.Errorw("could not get sequence number offset", err)
		return
	}

	r.lastSN = uint16(extPkt.ExtSequenceNumber - snOffset)
}

func (r *RTPMunger) UpdateAndGetSnTs(extPkt *buffer.ExtPacket) (*TranslationParamsRTP, error) {
	diff := extPkt.ExtSequenceNumber - r.extHighestIncomingSN
	if diff > (1 << 31) {
		// out-of-order, look up sequence number offset cache
		snOffset, err := r.snRangeMap.GetValue(extPkt.ExtSequenceNumber)
		if err != nil {
			return &TranslationParamsRTP{
				snOrdering: SequenceNumberOrderingOutOfOrder,
			}, ErrOutOfOrderSequenceNumberCacheMiss
		}

		return &TranslationParamsRTP{
			snOrdering:     SequenceNumberOrderingOutOfOrder,
			sequenceNumber: uint16(extPkt.ExtSequenceNumber - snOffset),
			timestamp:      extPkt.Packet.Timestamp - r.tsOffset,
		}, nil
	}

	// can get duplicate packet due to FEC
	if diff == 0 {
		return &TranslationParamsRTP{
			snOrdering: SequenceNumberOrderingDuplicate,
		}, ErrDuplicatePacket
	}

	ordering := SequenceNumberOrderingContiguous
	if diff > 1 {
		ordering = SequenceNumberOrderingGap
		r.snRangeMap.AddRange(r.extHighestIncomingSN+1, extPkt.ExtSequenceNumber)
	}

	r.extHighestIncomingSN = extPkt.ExtSequenceNumber

	// if padding only packet, can be dropped and sequence number adjusted, if contiguous
	if diff == 1 && len(extPkt.Packet.Payload) == 0 {
		r.snRangeMap.IncValue(1)
		return &TranslationParamsRTP{
			snOrdering: ordering,
		}, ErrPaddingOnlyPacket
	}

	snOffset, err := r.snRangeMap.GetValue(extPkt.ExtSequenceNumber)
	if err != nil {
		return &TranslationParamsRTP{
			snOrdering: ordering,
		}, ErrSequenceNumberOffsetNotFound
	}

	mungedSN := uint16(extPkt.ExtSequenceNumber - snOffset)
	mungedTS := extPkt.Packet.Timestamp - r.tsOffset

	r.lastSN = mungedSN
	r.lastTS = mungedTS
	r.lastMarker = extPkt.Packet.Marker

	if extPkt.KeyFrame {
		r.rtxGateSn = mungedSN
		r.isInRtxGateRegion = true
	}

	if r.isInRtxGateRegion && (mungedSN-r.rtxGateSn) < (1<<15) && (mungedSN-r.rtxGateSn) > RtxGateWindow {
		r.isInRtxGateRegion = false
	}

	return &TranslationParamsRTP{
		snOrdering:     ordering,
		sequenceNumber: mungedSN,
		timestamp:      mungedTS,
	}, nil
}

func (r *RTPMunger) FilterRTX(nacks []uint16) []uint16 {
	if !r.isInRtxGateRegion {
		return nacks
	}

	filtered := make([]uint16, 0, len(nacks))
	for _, sn := range nacks {
		if (sn - r.rtxGateSn) < (1 << 15) {
			filtered = append(filtered, sn)
		}
	}

	return filtered
}

func (r *RTPMunger) UpdateAndGetPaddingSnTs(num int, clockRate uint32, frameRate uint32, forceMarker bool, rtpTimestamp uint32) ([]SnTs, error) {
	useLastTSForFirst := false
	tsOffset := 0
	if !r.lastMarker {
		if !forceMarker {
			return nil, ErrPaddingNotOnFrameBoundary
		}

		// if forcing frame end, use timestamp of latest received frame for the first one
		useLastTSForFirst = true
		tsOffset = 1
	}

	lastTS := r.lastTS
	vals := make([]SnTs, num)
	for i := 0; i < num; i++ {
		vals[i].sequenceNumber = r.lastSN + uint16(i) + 1
		if frameRate != 0 {
			if useLastTSForFirst && i == 0 {
				vals[i].timestamp = r.lastTS
			} else {
				ts := rtpTimestamp + ((uint32(i+1-tsOffset)*clockRate)+frameRate-1)/frameRate
				if (ts-lastTS) == 0 || (ts-lastTS) > (1<<31) {
					ts = lastTS + 1
					lastTS = ts
				}
				vals[i].timestamp = ts
			}
		} else {
			vals[i].timestamp = r.lastTS
		}
	}

	r.lastSN = vals[num-1].sequenceNumber
	r.snRangeMap.DecValue(uint32(num))

	r.tsOffset -= vals[num-1].timestamp - r.lastTS
	r.lastTS = vals[num-1].timestamp

	if forceMarker {
		r.lastMarker = true
	}

	return vals, nil
}

func (r *RTPMunger) IsOnFrameBoundary() bool {
	return r.lastMarker
}
