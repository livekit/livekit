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
	ExtLastSN       uint64
	ExtSecondLastSN uint64
	ExtLastTS       uint64
}

func (r RTPMungerState) String() string {
	return fmt.Sprintf("RTPMungerState{extLastSN: %d, extSecondLastSN: %d, extLastTS: %d)", r.ExtLastSN, r.ExtSecondLastSN, r.ExtLastTS)
}

// ----------------------------------------------------------------------

type RTPMunger struct {
	logger logger.Logger

	extHighestIncomingSN uint64
	snRangeMap           *utils.RangeMap[uint64, uint64]

	extLastSN       uint64
	extSecondLastSN uint64
	extLastTS       uint64
	tsOffset        uint64
	lastMarker      bool

	extRtxGateSn      uint64
	isInRtxGateRegion bool
}

func NewRTPMunger(logger logger.Logger) *RTPMunger {
	return &RTPMunger{
		logger:     logger,
		snRangeMap: utils.NewRangeMap[uint64, uint64](100),
	}
}

func (r *RTPMunger) DebugInfo() map[string]interface{} {
	snOffset, _ := r.snRangeMap.GetValue(r.extHighestIncomingSN + 1)
	return map[string]interface{}{
		"ExtHighestIncomingSN": r.extHighestIncomingSN,
		"ExtLastSN":            r.extLastSN,
		"ExtSecondLastSN":      r.extSecondLastSN,
		"SNOffset":             snOffset,
		"ExtLastTS":            r.extLastTS,
		"TSOffset":             r.tsOffset,
		"LastMarker":           r.lastMarker,
	}
}

func (r *RTPMunger) GetLast() RTPMungerState {
	return RTPMungerState{
		ExtLastSN:       r.extLastSN,
		ExtSecondLastSN: r.extSecondLastSN,
		ExtLastTS:       r.extLastTS,
	}
}

func (r *RTPMunger) SeedLast(state RTPMungerState) {
	r.extLastSN = state.ExtLastSN
	r.extSecondLastSN = state.ExtSecondLastSN
	r.extLastTS = state.ExtLastTS
}

func (r *RTPMunger) SetLastSnTs(extPkt *buffer.ExtPacket) {
	r.extHighestIncomingSN = extPkt.ExtSequenceNumber - 1
	r.extLastSN = extPkt.ExtSequenceNumber
	r.extSecondLastSN = r.extLastSN - 1
	r.extLastTS = extPkt.ExtTimestamp
}

func (r *RTPMunger) UpdateSnTsOffsets(extPkt *buffer.ExtPacket, snAdjust uint64, tsAdjust uint64) {
	r.extHighestIncomingSN = extPkt.ExtSequenceNumber - 1
	r.snRangeMap.ClearAndResetValue(extPkt.ExtSequenceNumber - r.extLastSN - snAdjust)
	r.tsOffset = extPkt.ExtTimestamp - r.extLastTS - tsAdjust
}

func (r *RTPMunger) PacketDropped(extPkt *buffer.ExtPacket) {
	if r.extHighestIncomingSN != extPkt.ExtSequenceNumber {
		return
	}

	snOffset, err := r.snRangeMap.GetValue(extPkt.ExtSequenceNumber)
	if err == nil {
		outSN := extPkt.ExtSequenceNumber - snOffset
		if outSN != r.extLastSN {
			r.logger.Warnw("last outgoing sequence number mismatch", nil, "expected", r.extLastSN, "got", outSN)
		}
	}
	if r.extLastSN == r.extSecondLastSN {
		r.logger.Warnw("cannot roll back on drop", nil, "extLastSN", r.extLastSN, "secondLastSN", r.extSecondLastSN)
	}

	if err := r.snRangeMap.ExcludeRange(r.extHighestIncomingSN, r.extHighestIncomingSN+1); err != nil {
		r.logger.Errorw("could not exclude range", err, "sn", r.extHighestIncomingSN)
	}

	r.extLastSN = r.extSecondLastSN
}

func (r *RTPMunger) UpdateAndGetSnTs(extPkt *buffer.ExtPacket) (TranslationParamsRTP, error) {
	diff := int64(extPkt.ExtSequenceNumber - r.extHighestIncomingSN)

	// can get duplicate packet due to FEC
	if diff == 0 {
		return TranslationParamsRTP{
			snOrdering: SequenceNumberOrderingDuplicate,
		}, ErrDuplicatePacket
	}

	if diff < 0 {
		// out-of-order, look up sequence number offset cache
		snOffset, err := r.snRangeMap.GetValue(extPkt.ExtSequenceNumber)
		if err != nil {
			return TranslationParamsRTP{
				snOrdering: SequenceNumberOrderingOutOfOrder,
			}, ErrOutOfOrderSequenceNumberCacheMiss
		}

		return TranslationParamsRTP{
			snOrdering:     SequenceNumberOrderingOutOfOrder,
			sequenceNumber: uint16(extPkt.ExtSequenceNumber - snOffset),
			timestamp:      uint32(extPkt.ExtTimestamp - r.tsOffset),
		}, nil
	}

	ordering := SequenceNumberOrderingContiguous
	if diff > 1 {
		ordering = SequenceNumberOrderingGap
	}

	r.extHighestIncomingSN = extPkt.ExtSequenceNumber

	// if padding only packet, can be dropped and sequence number adjusted, if contiguous
	if diff == 1 && len(extPkt.Packet.Payload) == 0 {
		if err := r.snRangeMap.ExcludeRange(r.extHighestIncomingSN, r.extHighestIncomingSN+1); err != nil {
			r.logger.Errorw("could not exclude range", err, "sn", r.extHighestIncomingSN)
		}
		return TranslationParamsRTP{
			snOrdering: ordering,
		}, ErrPaddingOnlyPacket
	}

	snOffset, err := r.snRangeMap.GetValue(extPkt.ExtSequenceNumber)
	if err != nil {
		r.logger.Errorw("could not get sequence number adjustment", err, "sn", extPkt.ExtSequenceNumber, "payloadSize", len(extPkt.Packet.Payload))
		return TranslationParamsRTP{
			snOrdering: ordering,
		}, ErrSequenceNumberOffsetNotFound
	}

	extMungedSN := extPkt.ExtSequenceNumber - snOffset
	extMungedTS := extPkt.ExtTimestamp - r.tsOffset

	r.extSecondLastSN = r.extLastSN
	r.extLastSN = extMungedSN
	r.extLastTS = extMungedTS
	r.lastMarker = extPkt.Packet.Marker

	if extPkt.KeyFrame {
		r.extRtxGateSn = extMungedSN
		r.isInRtxGateRegion = true
	}

	if r.isInRtxGateRegion && (extMungedSN-r.extRtxGateSn) > RtxGateWindow {
		r.isInRtxGateRegion = false
	}

	return TranslationParamsRTP{
		snOrdering:     ordering,
		sequenceNumber: uint16(extMungedSN),
		timestamp:      uint32(extMungedTS),
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

func (r *RTPMunger) UpdateAndGetPaddingSnTs(num int, clockRate uint32, frameRate uint32, forceMarker bool, extRtpTimestamp uint64) ([]SnTs, error) {
	if num == 0 {
		return nil, nil
	}

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
	extLastTS := r.extLastTS
	vals := make([]SnTs, num)
	for i := 0; i < num; i++ {
		extLastSN++
		vals[i].sequenceNumber = uint16(extLastSN)

		if frameRate != 0 {
			if useLastTSForFirst && i == 0 {
				vals[i].timestamp = uint32(r.extLastTS)
			} else {
				ets := extRtpTimestamp + uint64(((uint32(i+1-tsOffset)*clockRate)+frameRate-1)/frameRate)
				if int64(ets-extLastTS) <= 0 {
					ets = extLastTS + 1
				}
				extLastTS = ets
				vals[i].timestamp = uint32(ets)
			}
		} else {
			vals[i].timestamp = uint32(r.extLastTS)
		}
	}

	r.extSecondLastSN = extLastSN - 1
	r.extLastSN = extLastSN
	r.snRangeMap.DecValue(uint64(num))

	r.tsOffset -= extLastTS - r.extLastTS
	r.extLastTS = extLastTS

	if forceMarker {
		r.lastMarker = true
	}

	return vals, nil
}

func (r *RTPMunger) IsOnFrameBoundary() bool {
	return r.lastMarker
}
