package sendsidebwe

import (
	"errors"
	"fmt"
	"time"
)

// -------------------------------------------------------------

var (
	errOutOfRange = errors.New("packet out of range")
)

// -------------------------------------------------------------

type PacketGroupParams struct {
	Spread time.Duration
}

type PacketGroup struct {
	params PacketGroupParams

	packetInfos []*packetInfo
	minSendTime int64
}

func NewPacketGroup(params PacketGroupParams) *PacketGroup {
	return &PacketGroup{
		params: params,
	}
}

// SSBWE-TODO - can get a really old packet that could spread things a bunch, how to deal with it?
func (p *PacketGroup) Add(pi *packetInfo) error {
	if p.minSendTime != 0 && (pi.sendTime-p.minSendTime) > p.params.Spread.Microseconds() {
		// SSBWE-TODO: add this packet also here to create overlap and leave out bytes from packet when calculating rates????
		return errOutOfRange
	}

	p.packetInfos = append(p.packetInfos, pi)
	if p.minSendTime == 0 || pi.sendTime < p.minSendTime {
		p.minSendTime = pi.sendTime
	}
	return nil
}

// SSBWE-TODO: re-evaluate functions that are really needed from here and also make as many as possible private
func (p *PacketGroup) GetMinSendTime() int64 {
	minSendTime := int64(0)
	for _, pi := range p.packetInfos {
		if (minSendTime == 0 && pi.sendTime != 0) || (pi.sendTime != 0 && pi.sendTime < minSendTime) {
			minSendTime = pi.sendTime
		}
	}

	return minSendTime
}

func (p *PacketGroup) GetMaxSendTime() int64 {
	maxSendTime := int64(0)
	for _, pi := range p.packetInfos {
		if pi.sendTime > maxSendTime {
			maxSendTime = pi.sendTime
		}
	}

	return maxSendTime
}

func (p *PacketGroup) GetSendSpread() int64 {
	maxSendTime := p.GetMaxSendTime()
	minSendTime := p.GetMinSendTime()
	if maxSendTime == 0 || minSendTime == 0 {
		return 0
	}

	return maxSendTime - minSendTime
}

func (p *PacketGroup) GetMinReceiveTime() int64 {
	minReceiveTime := int64(0)
	for _, pi := range p.packetInfos {
		if (minReceiveTime == 0 && pi.receiveTime != 0) || (pi.receiveTime != 0 && pi.receiveTime < minReceiveTime) {
			minReceiveTime = pi.receiveTime
		}
	}

	return minReceiveTime
}

func (p *PacketGroup) GetMaxReceiveTime() int64 {
	maxReceiveTime := int64(0)
	for _, pi := range p.packetInfos {
		if pi.receiveTime > maxReceiveTime {
			maxReceiveTime = pi.receiveTime
		}
	}

	return maxReceiveTime
}

func (p *PacketGroup) GetReceiveSpread() int64 {
	maxReceiveTime := p.GetMaxReceiveTime()
	minReceiveTime := p.GetMinReceiveTime()
	if maxReceiveTime == 0 || minReceiveTime == 0 {
		return 0
	}

	return maxReceiveTime - minReceiveTime
}

func (p *PacketGroup) String() string {
	numSendRTXPackets := 0
	sentHeaderBytes := 0
	sentPayloadBytes := 0
	sentHeaderRTXBytes := 0
	sentPayloadRTXBytes := 0

	numReceivedPackets := 0
	numReceivedRTXPackets := 0
	receivedHeaderBytes := 0
	receivedPayloadBytes := 0
	receivedHeaderRTXBytes := 0
	receivedPayloadRTXBytes := 0
	for _, pi := range p.packetInfos {
		if pi.isRTX {
			numSendRTXPackets++
			sentHeaderRTXBytes += int(pi.headerSize)
			sentPayloadRTXBytes += int(pi.payloadSize)
		} else {
			sentHeaderBytes += int(pi.headerSize)
			sentPayloadBytes += int(pi.payloadSize)
		}

		if pi.receiveTime != 0 {
			numReceivedPackets++
			if pi.isRTX {
				numReceivedRTXPackets++
				receivedHeaderRTXBytes += int(pi.headerSize)
				receivedPayloadRTXBytes += int(pi.payloadSize)
			} else {
				receivedHeaderBytes += int(pi.headerSize)
				receivedPayloadBytes += int(pi.payloadSize)
			}
		}
	}

	sendSpread := p.GetSendSpread()
	sendBitrate := float64(0)
	sendRTXBitrate := float64(0)
	if sendSpread != 0 {
		sendBitrate = float64(sentHeaderBytes+sentPayloadBytes) * 8 * 1_000_000 / float64(sendSpread)
		sendRTXBitrate = float64(sentHeaderRTXBytes+sentPayloadRTXBytes) * 8 * 1_000_000 / float64(sendSpread)
	}

	receiveSpread := p.GetReceiveSpread()
	receiveBitrate := float64(0)
	receiveRTXBitrate := float64(0)
	if receiveSpread != 0 {
		receiveBitrate = float64(receivedHeaderBytes+receivedPayloadBytes) * 8 * 1_000_000 / float64(receiveSpread)
		receiveRTXBitrate = float64(receivedHeaderRTXBytes+receivedPayloadRTXBytes) * 8 * 1_000_000 / float64(receiveSpread)
	}

	ratio := float64(0)
	if sendSpread > 0 {
		ratio = float64(receiveSpread) / float64(sendSpread)
	}
	return fmt.Sprintf("send: (%d / %d / %d) / (%d / %d / %d) / (%d / %d / %d) / (%.2f / %.2f), receive: (%d / %d / %d) / (%d / %d / %d) / (%d / %d / %d) /  (%.2f / %.2f), ratio: %.2f",
		len(p.packetInfos), sentHeaderBytes, sentPayloadBytes, numSendRTXPackets, sentHeaderRTXBytes, sentPayloadRTXBytes, p.GetMinSendTime(), p.GetMaxSendTime(), sendSpread, sendBitrate, sendRTXBitrate,
		numReceivedPackets, receivedHeaderBytes, receivedPayloadBytes, numReceivedRTXPackets, receivedHeaderRTXBytes, receivedPayloadRTXBytes, p.GetMinReceiveTime(), p.GetMaxReceiveTime(), receiveSpread, receiveBitrate, receiveRTXBitrate,
		ratio,
	)
}
