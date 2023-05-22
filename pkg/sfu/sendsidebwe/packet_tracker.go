package sendsidebwe

import (
	"sync"
	"time"

	"github.com/MicahParks/peakdetect"
	"github.com/livekit/protocol/logger"
	"github.com/pion/rtcp"
)

type packetInfo struct {
	sendTime time.Time
	// RAJA-TODO: may need a feedback report time to detect stale reports
	arrivalTime int64
	headerSize  uint16
	payloadSize uint16
}

type PacketTracker struct {
	logger logger.Logger

	lock            sync.RWMutex
	sentInitialized bool
	highestSentSN   uint16

	ackedInitialized bool
	highestAckedSN   uint16

	// RAJA-TODO: make this a ring buffer as a lot more fields are needed
	packetInfos [1 << 16]packetInfo

	peakDetector peakdetect.PeakDetector
}

func NewPacketTracker(logger logger.Logger) *PacketTracker {
	p := &PacketTracker{
		logger:       logger,
		peakDetector: peakdetect.NewPeakDetector(),
	}

	// RAJA-TODO: make consts
	p.peakDetector.Initialize(0.1, 3.5, make([]float64, 60))
	return p
}

func (p *PacketTracker) PacketSent(sn uint16, at time.Time, headerSize int, payloadSize int) {
	p.lock.Lock()
	defer p.lock.Unlock()

	if !p.sentInitialized {
		p.highestSentSN = sn - 1
		p.sentInitialized = true
	}

	// should never happen, but just a sanity check
	if (sn - p.highestSentSN) > (1 << 15) {
		return
	}

	// clear slots occupied by missing packets,
	// ideally this should never run as seequence numbers should be generated in order
	// and packets sent in order.
	for i := p.highestSentSN + 1; i != sn; i++ {
		pi := &p.packetInfos[i]
		pi.sendTime = time.Time{}
		pi.headerSize = 0
		pi.payloadSize = 0
	}

	pi := &p.packetInfos[sn]
	pi.sendTime = at
	pi.headerSize = uint16(headerSize)
	pi.payloadSize = uint16(payloadSize)

	p.highestSentSN = sn
}

func (p *PacketTracker) ProcessFeedback(baseSN uint16, arrivals []int64) {
	p.lock.Lock()
	defer p.lock.Unlock()

	for i, arrival := range arrivals {
		p.packetInfos[baseSN+uint16(i)].arrivalTime = arrival
	}

	lastAckedSN := baseSN + uint16(len(arrivals)) - 1
	if !p.ackedInitialized {
		p.highestAckedSN = lastAckedSN
		p.ackedInitialized = true
	} else {
		if (lastAckedSN - p.highestAckedSN) < (1 << 15) {
			p.highestAckedSN = lastAckedSN
		}
	}

	// RAJA-TODO send notification to worker to update acknowledged bitrate
	p.calculateAcknowledgedBitrate() // RAJA-TODO: this should run in worker

	deltas := p.getDeltas(baseSN, baseSN+uint16(len(arrivals)))
	signals := p.peakDetector.NextBatch(deltas)
	p.logger.Infow("peaks", "deltas", deltas, "signals", signals) // REMOVE

	p.calculateMPE() // RAJA-TODO: this should run in worker
}

func (p *PacketTracker) getDeltas(startSNInclusive, endSNExclusive uint16) []float64 {
	deltas := make([]float64, 0, endSNExclusive-startSNInclusive)
	for sn := startSNInclusive; sn != endSNExclusive; sn++ {
		pi := &p.packetInfos[sn]
		piPrev := &p.packetInfos[sn-1]
		if pi.sendTime.IsZero() || piPrev.sendTime.IsZero() {
			break
		}

		// lost packet(s)
		if pi.arrivalTime == 0 || piPrev.arrivalTime == 0 {
			continue
		}

		// ignore out-of-order arrivals
		if piPrev.arrivalTime > pi.arrivalTime {
			continue
		}

		sendDiff := pi.sendTime.Sub(piPrev.sendTime).Microseconds()
		arrivalDiff := pi.arrivalTime - piPrev.arrivalTime
		delta := arrivalDiff - sendDiff
		if delta < 0 && delta > -rtcp.TypeTCCDeltaScaleFactor {
			// TWCC feedback has a resolution of 250 us inter packet interval,
			// squash small send intervals getting coalesced on the receiver side.
			delta = 0
		}
		deltas = append(deltas, float64(delta))
		p.logger.Infow("delta", "sn", sn, "send", pi.sendTime, "sendp", piPrev.sendTime, "sd", sendDiff, "a", pi.arrivalTime, "ap", piPrev.arrivalTime, "adiff", arrivalDiff, "diff", arrivalDiff-sendDiff, "delta", delta) // REMOVE
	}

	return deltas
}

func (p *PacketTracker) calculateMPE() {
	if !p.ackedInitialized {
		return
	}

	sn := p.highestAckedSN
	endTime := p.packetInfos[sn].arrivalTime
	startTime := endTime - 500000 // RAJA-TODO - make this constant and tune for rate calculation
	// RAJA-TODO: should this window be dynamic?

	totalError := float64(0.0)
	numDeltas := 0
	for {
		pi := &p.packetInfos[sn]
		piPrev := &p.packetInfos[sn-1]
		if pi.sendTime.IsZero() || piPrev.sendTime.IsZero() {
			break
		}

		// lost packet(s)
		if pi.arrivalTime == 0 || piPrev.arrivalTime == 0 {
			continue
		}

		// ignore out-of-order arrivals
		if piPrev.arrivalTime > pi.arrivalTime {
			continue
		}

		if pi.arrivalTime < startTime || pi.sendTime.IsZero() {
			break
		}

		sendDiff := pi.sendTime.Sub(piPrev.sendTime).Microseconds()
		arrivalDiff := pi.arrivalTime - piPrev.arrivalTime
		delta := arrivalDiff - sendDiff
		if delta < 0 && delta > -rtcp.TypeTCCDeltaScaleFactor {
			// TWCC feedback has a resolution of 250 us inter packet interval,
			// squash small send intervals getting coalesced on the receiver side.
			delta = 0
		}
		if arrivalDiff != 0 {
			totalError += float64(delta) / float64(arrivalDiff)
			numDeltas++
			p.logger.Infow("mpe delta", "sn", sn, "send", pi.sendTime, "sendp", piPrev.sendTime, "sd", sendDiff, "a", pi.arrivalTime, "ap", piPrev.arrivalTime, "adiff", arrivalDiff, "diff", arrivalDiff-sendDiff, "delta", delta, "error", float64(delta)/float64(arrivalDiff)) // REMOVE
		}
		sn--
	}

	if numDeltas == 0 {
		return
	}

	mpe := float64(totalError) / float64(numDeltas) * 100.0
	p.logger.Infow("mpe", "totalError", totalError, "numDeltas", numDeltas, "mpe", mpe) // REMOVE
}

func (p *PacketTracker) calculateAcknowledgedBitrate() {
	if !p.ackedInitialized {
		return
	}

	sn := p.highestAckedSN
	endTime := p.packetInfos[sn].arrivalTime
	startTime := endTime - 500000 // RAJA-TODO - make this constant and tune for rate calculation
	// RAJA-TODO: should this window be dynamic?

	highestTime := endTime
	lowestTime := int64(0)
	lowestTimeBytes := 0

	totalBytes := 0
	numPackets := 0
	for {
		pi := &p.packetInfos[sn]
		arrivalTime := pi.arrivalTime
		if arrivalTime == 0 && !pi.sendTime.IsZero() {
			// lost packet or not sent packet
			sn--
			continue
			// RAJA-TODO think about whether lost packet should be counted for bitrate calculation
		}

		if arrivalTime > endTime {
			// late arriving (out-of-order arriving) packet
			sn--
			continue
		}

		if arrivalTime < startTime || pi.sendTime.IsZero() {
			break
		}

		if arrivalTime > highestTime {
			highestTime = arrivalTime
		}
		if lowestTime == 0 || arrivalTime < lowestTime {
			lowestTime = arrivalTime
			lowestTimeBytes = int(pi.headerSize) + int(pi.payloadSize)
		}

		totalBytes += int(pi.headerSize) + int(pi.payloadSize)
		numPackets++
		sn--
	}

	interval := float64(highestTime-lowestTime) / 1e6
	if interval == 0 || numPackets < 2 {
		return
	}

	// take out the edge
	numPackets--
	totalBytes -= lowestTimeBytes

	bitrate := float64(totalBytes) * 8 / interval
	packetRate := float64(numPackets) / interval
	p.logger.Infow("bitrate calculation",
		"totalBytes", totalBytes,
		"highest", highestTime,
		"lowest", lowestTime,
		"interval", interval,
		"bitrate", bitrate,
		"numPackets", numPackets,
		"packetRate", packetRate,
	) // REMOVE
}

// ------------------------------------------------
