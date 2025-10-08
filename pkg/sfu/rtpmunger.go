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
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/sfu/utils"
)

// RTPMunger
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
	snOrdering        SequenceNumberOrdering
	extSequenceNumber uint64
	extTimestamp      uint64
}

type SnTs struct {
	extSequenceNumber uint64
	extTimestamp      uint64
}

// ----------------------------------------------------------------------

type RTPMunger struct {
	logger logger.Logger

	extHighestIncomingSN uint64
	snRangeMap           *utils.RangeMap[uint64, uint64]

	extLastSN       uint64
	extSecondLastSN uint64
	snOffset        uint64

	extLastTS       uint64
	extSecondLastTS uint64
	tsOffset        uint64

	lastMarker       bool
	secondLastMarker bool

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
	return map[string]interface{}{
		"ExtHighestIncomingSN": r.extHighestIncomingSN,
		"ExtLastSN":            r.extLastSN,
		"ExtSecondLastSN":      r.extSecondLastSN,
		"SNOffset":             r.snOffset,
		"ExtLastTS":            r.extLastTS,
		"ExtSecondLastTS":      r.extSecondLastTS,
		"TSOffset":             r.tsOffset,
		"LastMarker":           r.lastMarker,
		"SecondLastMarker":     r.secondLastMarker,
	}
}

func (r *RTPMunger) GetState() *livekit.RTPMungerState {
	return &livekit.RTPMungerState{
		ExtLastSequenceNumber:       r.extLastSN,
		ExtSecondLastSequenceNumber: r.extSecondLastSN,
		ExtLastTimestamp:            r.extLastTS,
		ExtSecondLastTimestamp:      r.extSecondLastTS,
		LastMarker:                  r.lastMarker,
		SecondLastMarker:            r.secondLastMarker,
	}
}

func (r *RTPMunger) GetTSOffset() uint64 {
	return r.tsOffset
}

func (r *RTPMunger) SeedState(state *livekit.RTPMungerState) {
	r.extLastSN = state.ExtLastSequenceNumber
	r.extSecondLastSN = state.ExtSecondLastSequenceNumber
	r.extLastTS = state.ExtLastTimestamp
	r.extSecondLastTS = state.ExtSecondLastTimestamp
	r.lastMarker = state.LastMarker
	r.secondLastMarker = state.SecondLastMarker
}

func (r *RTPMunger) SetLastSnTs(extPkt *buffer.ExtPacket) {
	r.extHighestIncomingSN = extPkt.ExtSequenceNumber - 1

	r.extLastSN = extPkt.ExtSequenceNumber
	r.extSecondLastSN = r.extLastSN - 1
	r.snRangeMap.ClearAndResetValue(extPkt.ExtSequenceNumber, 0)
	r.updateSnOffset()

	r.extLastTS = extPkt.ExtTimestamp
	r.extSecondLastTS = extPkt.ExtTimestamp
	r.tsOffset = 0
}

func (r *RTPMunger) UpdateSnTsOffsets(extPkt *buffer.ExtPacket, snAdjust uint64, tsAdjust uint64) {
	r.extHighestIncomingSN = extPkt.ExtSequenceNumber - 1

	r.snRangeMap.ClearAndResetValue(extPkt.ExtSequenceNumber, extPkt.ExtSequenceNumber-r.extLastSN-snAdjust)
	r.updateSnOffset()

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
	r.updateSnOffset()

	r.extLastTS = r.extSecondLastTS
	r.lastMarker = r.secondLastMarker
}

func (r *RTPMunger) UpdateAndGetSnTs(extPkt *buffer.ExtPacket, marker bool) (TranslationParamsRTP, error) {
	diff := int64(extPkt.ExtSequenceNumber - r.extHighestIncomingSN)
	if (diff == 1 && len(extPkt.Packet.Payload) != 0) || diff > 1 {
		// in-order - either contiguous packet with payload OR packet following a gap, may or may not have payload
		r.extHighestIncomingSN = extPkt.ExtSequenceNumber

		ordering := SequenceNumberOrderingContiguous
		if diff > 1 {
			ordering = SequenceNumberOrderingGap
		}

		extMungedSN := extPkt.ExtSequenceNumber - r.snOffset
		extMungedTS := extPkt.ExtTimestamp - r.tsOffset

		r.extSecondLastSN = r.extLastSN
		r.extLastSN = extMungedSN
		r.extSecondLastTS = r.extLastTS
		r.extLastTS = extMungedTS
		r.secondLastMarker = r.lastMarker
		r.lastMarker = marker

		if extPkt.KeyFrame {
			r.extRtxGateSn = extMungedSN
			r.isInRtxGateRegion = true
		}

		if r.isInRtxGateRegion && (extMungedSN-r.extRtxGateSn) > RtxGateWindow {
			r.isInRtxGateRegion = false
		}

		return TranslationParamsRTP{
			snOrdering:        ordering,
			extSequenceNumber: extMungedSN,
			extTimestamp:      extMungedTS,
		}, nil
	}

	if diff < 0 {
		// out-of-order, look up sequence number offset cache
		snOffset, err := r.snRangeMap.GetValue(extPkt.ExtSequenceNumber)
		if err != nil {
			return TranslationParamsRTP{
				snOrdering: SequenceNumberOrderingOutOfOrder,
			}, ErrOutOfOrderSequenceNumberCacheMiss
		}

		extSequenceNumber := extPkt.ExtSequenceNumber - snOffset
		if extSequenceNumber >= r.extLastSN {
			// should not happen, just being paranoid
			r.logger.Errorw(
				"unexpected packet ordering", nil,
				"extIncomingSN", extPkt.ExtSequenceNumber,
				"extHighestIncomingSN", r.extHighestIncomingSN,
				"extLastSN", r.extLastSN,
				"snOffsetIncoming", snOffset,
				"snOffsetHighest", r.snOffset,
			)
			return TranslationParamsRTP{
				snOrdering: SequenceNumberOrderingOutOfOrder,
			}, ErrOutOfOrderSequenceNumberCacheMiss
		}

		return TranslationParamsRTP{
			snOrdering:        SequenceNumberOrderingOutOfOrder,
			extSequenceNumber: extSequenceNumber,
			extTimestamp:      extPkt.ExtTimestamp - r.tsOffset,
		}, nil
	}

	// if padding only packet, can be dropped and sequence number adjusted, if contiguous
	if diff == 1 {
		r.extHighestIncomingSN = extPkt.ExtSequenceNumber

		if err := r.snRangeMap.ExcludeRange(r.extHighestIncomingSN, r.extHighestIncomingSN+1); err != nil {
			r.logger.Errorw("could not exclude range", err, "sn", r.extHighestIncomingSN)
		}

		r.updateSnOffset()

		return TranslationParamsRTP{
			snOrdering: SequenceNumberOrderingContiguous,
		}, ErrPaddingOnlyPacket
	}

	// can get duplicate packet due to FEC
	return TranslationParamsRTP{
		snOrdering: SequenceNumberOrderingDuplicate,
	}, ErrDuplicatePacket
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

func (r *RTPMunger) UpdateAndGetPaddingSnTs(
	num int,
	clockRate uint32,
	frameRate uint32,
	forceMarker bool,
	extRtpTimestamp uint64,
) ([]SnTs, error) {
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
		vals[i].extSequenceNumber = extLastSN

		if frameRate != 0 {
			if useLastTSForFirst && i == 0 {
				vals[i].extTimestamp = r.extLastTS
			} else {
				ets := extRtpTimestamp + uint64(((uint32(i+1-tsOffset)*clockRate)+frameRate-1)/frameRate)
				if int64(ets-extLastTS) <= 0 {
					ets = extLastTS + 1
				}
				extLastTS = ets
				vals[i].extTimestamp = ets
			}
		} else {
			vals[i].extTimestamp = r.extLastTS
		}
	}

	r.extSecondLastSN = extLastSN - 1
	r.extLastSN = extLastSN
	r.snRangeMap.DecValue(r.extHighestIncomingSN, uint64(num))
	r.updateSnOffset()

	if len(vals) == 1 {
		r.extSecondLastTS = r.extLastTS
	} else {
		r.extSecondLastTS = vals[len(vals)-2].extTimestamp
	}
	r.tsOffset -= extLastTS - r.extLastTS
	r.extLastTS = extLastTS

	r.secondLastMarker = r.lastMarker
	if forceMarker {
		r.lastMarker = true
	}

	return vals, nil
}

func (r *RTPMunger) IsOnFrameBoundary() bool {
	return r.lastMarker
}

func (r *RTPMunger) updateSnOffset() {
	snOffset, err := r.snRangeMap.GetValue(r.extHighestIncomingSN + 1)
	if err != nil {
		r.logger.Errorw("could not get sequence number offset", err)
	}
	r.snOffset = snOffset
}
