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
	ExtLastSN uint32
	LastTS    uint32
}

func (r RTPMungerState) String() string {
	return fmt.Sprintf("RTPMungerState{extLastSN: %d, lastTS: %d)", r.ExtLastSN, r.LastTS)
}

// ----------------------------------------------------------------------

type RTPMunger struct {
	logger logger.Logger

	extHighestIncomingSN uint32
	snRangeMap           *utils.RangeMap[uint32, uint32]

	extLastSN  uint32
	lastTS     uint32
	tsOffset   uint32
	lastMarker bool

	extRtxGateSn      uint32
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
		"ExtLastSN":            r.extLastSN,
		"SNOffset":             snOffset,
		"LastTS":               r.lastTS,
		"TSOffset":             r.tsOffset,
		"LastMarker":           r.lastMarker,
	}
}

func (r *RTPMunger) GetLast() RTPMungerState {
	return RTPMungerState{
		ExtLastSN: r.extLastSN,
		LastTS:    r.lastTS,
	}
}

func (r *RTPMunger) SeedLast(state RTPMungerState) {
	r.extLastSN = state.ExtLastSN
	r.lastTS = state.LastTS
}

func (r *RTPMunger) SetLastSnTs(extPkt *buffer.ExtPacket) {
	r.extHighestIncomingSN = extPkt.ExtSequenceNumber - 1
	r.extLastSN = extPkt.ExtSequenceNumber
	r.lastTS = extPkt.Packet.Timestamp
}

func (r *RTPMunger) UpdateSnTsOffsets(extPkt *buffer.ExtPacket, snAdjust uint32, tsAdjust uint32) {
	r.extHighestIncomingSN = extPkt.ExtSequenceNumber - 1
	r.snRangeMap.ClearAndResetValue(extPkt.ExtSequenceNumber - r.extLastSN - snAdjust)
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

	r.extLastSN = extPkt.ExtSequenceNumber - snOffset
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

	extMungedSN := extPkt.ExtSequenceNumber - snOffset
	mungedTS := extPkt.Packet.Timestamp - r.tsOffset

	r.extLastSN = extMungedSN
	r.lastTS = mungedTS
	r.lastMarker = extPkt.Packet.Marker

	if extPkt.KeyFrame {
		r.extRtxGateSn = extMungedSN
		r.isInRtxGateRegion = true
	}

	if r.isInRtxGateRegion && (extMungedSN-r.extRtxGateSn) > RtxGateWindow {
		r.isInRtxGateRegion = false
	}

	return &TranslationParamsRTP{
		snOrdering:     ordering,
		sequenceNumber: uint16(extMungedSN),
		timestamp:      mungedTS,
	}, nil
}

func (r *RTPMunger) FilterRTX(nacks []uint16) []uint16 {
	if !r.isInRtxGateRegion {
		return nacks
	}

	filtered := make([]uint16, 0, len(nacks))
	for _, sn := range nacks {
		if (sn - uint16(r.extRtxGateSn)) < (1 << 15) {
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

	extLastSN := r.extLastSN
	lastTS := r.lastTS
	vals := make([]SnTs, num)
	for i := 0; i < num; i++ {
		extLastSN++
		vals[i].sequenceNumber = uint16(extLastSN)
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

	r.extLastSN = extLastSN
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
