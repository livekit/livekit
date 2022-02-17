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

type RTPMungerParams struct {
	highestIncomingSN uint16
	lastSN            uint16
	snOffset          uint16
	lastTS            uint32
	tsOffset          uint32
	lastMarker        bool

	missingSNs map[uint16]uint16

	rtxGateSn uint16
	isInRtxGateRegion bool
}

type RTPMunger struct {
	logger logger.Logger

	RTPMungerParams
}

func NewRTPMunger(logger logger.Logger) *RTPMunger {
	return &RTPMunger{
		logger: logger,
		RTPMungerParams: RTPMungerParams{
			missingSNs: make(map[uint16]uint16, 10),
		},
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

func (r *RTPMunger) SetLastSnTs(extPkt *buffer.ExtPacket) {
	r.highestIncomingSN = extPkt.Packet.SequenceNumber - 1
	r.lastSN = extPkt.Packet.SequenceNumber
	r.lastTS = extPkt.Packet.Timestamp
}

func (r *RTPMunger) UpdateSnTsOffsets(extPkt *buffer.ExtPacket, snAdjust uint16, tsAdjust uint32) {
	r.highestIncomingSN = extPkt.Packet.SequenceNumber - 1
	r.snOffset = extPkt.Packet.SequenceNumber - r.lastSN - snAdjust
	r.tsOffset = extPkt.Packet.Timestamp - r.lastTS - tsAdjust

	// clear incoming missing sequence numbers on layer/source switch
	r.missingSNs = make(map[uint16]uint16, 10)
}

func (r *RTPMunger) PacketDropped(extPkt *buffer.ExtPacket) {
	if !extPkt.Head {
		return
	}

	r.highestIncomingSN = extPkt.Packet.SequenceNumber
	r.snOffset += 1
	r.lastSN = extPkt.Packet.SequenceNumber - r.snOffset
}

func (r *RTPMunger) UpdateAndGetSnTs(extPkt *buffer.ExtPacket) (*TranslationParamsRTP, error) {
	// if out-of-order, look up missing sequence number cache
	if !extPkt.Head {
		snOffset, ok := r.missingSNs[extPkt.Packet.SequenceNumber]
		if !ok {
			return &TranslationParamsRTP{
				snOrdering: SequenceNumberOrderingOutOfOrder,
			}, ErrOutOfOrderSequenceNumberCacheMiss
		}

		delete(r.missingSNs, extPkt.Packet.SequenceNumber)
		return &TranslationParamsRTP{
			snOrdering:     SequenceNumberOrderingOutOfOrder,
			sequenceNumber: extPkt.Packet.SequenceNumber - snOffset,
			timestamp:      extPkt.Packet.Timestamp - r.tsOffset,
		}, nil
	}

	ordering := SequenceNumberOrderingContiguous

	// if there are gaps, record it in missing sequence number cache
	diff := extPkt.Packet.SequenceNumber - r.highestIncomingSN
	if diff > 1 {
		ordering = SequenceNumberOrderingGap

		for i := r.highestIncomingSN + 1; i != extPkt.Packet.SequenceNumber; i++ {
			r.missingSNs[i] = r.snOffset
		}
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

	if r.isInRtxGateRegion && (mungedSN - r.rtxGateSn) > RtxGateWindow {
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

	if forceMarker {
		r.lastMarker = true
	}

	return vals, nil
}

func (r *RTPMunger) IsOnFrameBoundary() bool {
	return r.lastMarker
}
