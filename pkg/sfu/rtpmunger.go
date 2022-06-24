package sfu

import (
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
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

type RTPMungerState struct {
	LastSN uint16
	LastTS uint32
}

type RTPMungerParams struct {
	highestIncomingSN uint16
	lastSN            uint16
	snOffset          uint16
	lastTS            uint32
	tsOffset          uint32
	lastMarker        bool

	snOffsets          [SnOffsetCacheSize]uint16
	snOffsetsWritePtr  int
	snOffsetsOccupancy int

	rtxGateSn         uint16
	isInRtxGateRegion bool
}

type RTPMunger struct {
	logger logger.Logger

	RTPMungerParams
}

func NewRTPMunger(logger logger.Logger) *RTPMunger {
	return &RTPMunger{
		logger: logger,
	}
}

func (r *RTPMunger) GetParams() RTPMungerParams {
	return RTPMungerParams{
		highestIncomingSN: r.highestIncomingSN,
		lastSN:            r.lastSN,
		snOffset:          r.snOffset,
		lastTS:            r.lastTS,
		tsOffset:          r.tsOffset,
		lastMarker:        r.lastMarker,
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
	r.highestIncomingSN = extPkt.Packet.SequenceNumber - 1
	r.lastSN = extPkt.Packet.SequenceNumber
	r.lastTS = extPkt.Packet.Timestamp
}

func (r *RTPMunger) UpdateSnTsOffsets(extPkt *buffer.ExtPacket, snAdjust uint16, tsAdjust uint32) {
	r.highestIncomingSN = extPkt.Packet.SequenceNumber - 1
	r.snOffset = extPkt.Packet.SequenceNumber - r.lastSN - snAdjust
	r.tsOffset = extPkt.Packet.Timestamp - r.lastTS - tsAdjust

	// clear offsets cache layer/source switch
	r.snOffsetsWritePtr = 0
	r.snOffsetsOccupancy = 0
}

func (r *RTPMunger) PacketDropped(extPkt *buffer.ExtPacket) {
	if r.highestIncomingSN != extPkt.Packet.SequenceNumber {
		return
	}
	r.snOffset += 1
	r.lastSN = extPkt.Packet.SequenceNumber - r.snOffset

	r.snOffsetsWritePtr = (r.snOffsetsWritePtr - 1) & SnOffsetCacheMask
	r.snOffsetsOccupancy--
	if r.snOffsetsOccupancy < 0 {
		r.logger.Warnw("sequence number offset cache is invalid", nil, "occupancy", r.snOffsetsOccupancy)
	}
}

func (r *RTPMunger) UpdateAndGetSnTs(extPkt *buffer.ExtPacket) (*TranslationParamsRTP, error) {
	// if out-of-order, look up sequence number offset cache
	diff := extPkt.Packet.SequenceNumber - r.highestIncomingSN
	if diff > (1 << 15) {
		snOffset, isValid := r.getSnOffset(extPkt.Packet.SequenceNumber)
		if !isValid {
			return &TranslationParamsRTP{
				snOrdering: SequenceNumberOrderingOutOfOrder,
			}, ErrOutOfOrderSequenceNumberCacheMiss
		}

		return &TranslationParamsRTP{
			snOrdering:     SequenceNumberOrderingOutOfOrder,
			sequenceNumber: extPkt.Packet.SequenceNumber - snOffset,
			timestamp:      extPkt.Packet.Timestamp - r.tsOffset,
		}, nil
	}

	ordering := SequenceNumberOrderingContiguous

	if diff > 1 {
		ordering = SequenceNumberOrderingGap
	} else {
		// can get duplicate packet due to FEC
		if diff == 0 {
			return &TranslationParamsRTP{
				snOrdering: SequenceNumberOrderingDuplicate,
			}, ErrDuplicatePacket
		}

		// if padding only packet, can be dropped and sequence number adjusted
		// as it is contiguous and in order. That means this is the highest
		// incoming sequence number, and it is a good point to adjust
		// sequence number offset.
		if len(extPkt.Packet.Payload) == 0 {
			r.highestIncomingSN = extPkt.Packet.SequenceNumber
			r.snOffset += 1

			return &TranslationParamsRTP{
				snOrdering: SequenceNumberOrderingContiguous,
			}, ErrPaddingOnlyPacket
		}
	}

	// record sn offset
	for i := r.highestIncomingSN + 1; i != extPkt.Packet.SequenceNumber+1; i++ {
		r.snOffsets[r.snOffsetsWritePtr] = r.snOffset
		r.snOffsetsWritePtr = (r.snOffsetsWritePtr + 1) & SnOffsetCacheMask
		r.snOffsetsOccupancy++
	}
	if r.snOffsetsOccupancy > SnOffsetCacheSize {
		r.snOffsetsOccupancy = SnOffsetCacheSize
	}

	// in-order incoming packet, may or may not be contiguous.
	// In the case of loss (i.e. incoming sequence number is not contiguous),
	// forward even if it is a padding only packet. With temporal scalability,
	// it is unclear if the current packet should be dropped if it is not
	// contiguous. Hence, forward anything that is not contiguous.
	// Reference: http://www.rtcbits.com/2017/04/howto-implement-temporal-scalability.html
	mungedSN := extPkt.Packet.SequenceNumber - r.snOffset
	mungedTS := extPkt.Packet.Timestamp - r.tsOffset

	r.highestIncomingSN = extPkt.Packet.SequenceNumber
	r.lastSN = mungedSN
	r.lastTS = mungedTS
	r.lastMarker = extPkt.Packet.Marker

	if extPkt.KeyFrame {
		r.rtxGateSn = mungedSN
		r.isInRtxGateRegion = true
	}

	if r.isInRtxGateRegion && (mungedSN-r.rtxGateSn) > RtxGateWindow {
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

func (r *RTPMunger) UpdateAndGetPaddingSnTs(num int, clockRate uint32, frameRate uint32, forceMarker bool) ([]SnTs, error) {
	tsOffset := 0
	if !r.lastMarker {
		if !forceMarker {
			return nil, ErrPaddingNotOnFrameBoundary
		} else {
			// if forcing frame end, use timestamp of latest received frame for the first one
			tsOffset = 1
		}
	}

	vals := make([]SnTs, num)
	for i := 0; i < num; i++ {
		vals[i].sequenceNumber = r.lastSN + uint16(i) + 1
		if frameRate != 0 {
			vals[i].timestamp = r.lastTS + uint32(i+1-tsOffset)*(clockRate/frameRate)
		} else {
			vals[i].timestamp = r.lastTS
		}
	}

	r.lastSN = vals[num-1].sequenceNumber
	r.snOffset -= uint16(num)

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

func (r *RTPMunger) getSnOffset(sn uint16) (uint16, bool) {
	diff := r.highestIncomingSN - sn
	if int(diff) >= r.snOffsetsOccupancy {
		return 0, false
	}

	readPtr := (r.snOffsetsWritePtr - int(diff) - 1) & SnOffsetCacheMask
	return r.snOffsets[readPtr], true
}
