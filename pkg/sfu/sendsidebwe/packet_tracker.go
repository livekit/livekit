package sendsidebwe

import (
	"sync"
	"time"

	"github.com/MicahParks/peakdetect"
	"github.com/frostbyte73/core"
	"github.com/gammazero/deque"
	"github.com/livekit/protocol/logger"
	"github.com/pion/rtcp"
)

type packetInfo struct {
	sendTime time.Time
	// SSBWE-TODO: may need a feedback report time to detect stale reports
	receiveTime int64
	headerSize  uint16
	payloadSize uint16
	// SSBWE-TODO: possibly add the following fields - pertaining to this packet,
	// idea is to be able to traverse back and find last packet with clean signal(s)
	// in order to figure out bitrate at which congestion triggered.
	//    MPETau - Mean Percentage Error trend - potentially an early signal of impending congestion
	//    PeakDetectorSignal - A sustained peak could indicate definite congestion
	//    AcknowledgedBitrate - Bitrate acknowledged by feedback
	//    ProbePacketInfo - When probing, these packets could be analyzed separately to check if probing is successful
}

type feedbackInfo struct {
	baseSN     uint16
	numPackets uint16
}

type PacketTracker struct {
	logger logger.Logger

	lock            sync.RWMutex
	sentInitialized bool
	highestSentSN   uint16

	ackedInitialized bool
	highestAckedSN   uint16

	// SSBWE-TODO: make this a ring buffer as a lot more fields are needed
	packetInfos   [1 << 16]packetInfo
	feedbackInfos deque.Deque[feedbackInfo]

	peakDetector peakdetect.PeakDetector

	wake chan struct{}
	stop core.Fuse
}

func NewPacketTracker(logger logger.Logger) *PacketTracker {
	p := &PacketTracker{
		logger:       logger,
		peakDetector: peakdetect.NewPeakDetector(),
		wake:         make(chan struct{}, 1),
		stop:         core.NewFuse(),
	}

	// SSBWE-TODO: make consts
	p.peakDetector.Initialize(0.1, 3.5, make([]float64, 60))

	p.feedbackInfos.SetMinCapacity(3)

	go p.worker()
	return p
}

func (p *PacketTracker) Stop() {
	p.stop.Once(func() {
		close(p.wake)
	})
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
		p.packetInfos[baseSN+uint16(i)].receiveTime = arrival
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

	p.feedbackInfos.PushBack(feedbackInfo{
		baseSN:     baseSN,
		numPackets: uint16(len(arrivals)),
	})
	// notify worker of a new feedback
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *PacketTracker) detectChangePoint(startSNInclusive, endSNExclusive uint16) {
	p.lock.RLock()
	defer p.lock.RUnlock()

	deltas := make([]float64, 0, endSNExclusive-startSNInclusive)
	for sn := startSNInclusive; sn != endSNExclusive; sn++ {
		pi := &p.packetInfos[sn]
		piPrev := &p.packetInfos[sn-1]
		if pi.sendTime.IsZero() || piPrev.sendTime.IsZero() {
			break
		}

		// lost packet(s)
		if pi.receiveTime == 0 || piPrev.receiveTime == 0 {
			continue
		}

		// ignore out-of-order arrivals
		if piPrev.receiveTime > pi.receiveTime {
			continue
		}

		sendDelta := pi.sendTime.Sub(piPrev.sendTime).Microseconds()
		receiveDelta := pi.receiveTime - piPrev.receiveTime
		delta := receiveDelta - sendDelta
		if delta < 0 && delta > -rtcp.TypeTCCDeltaScaleFactor {
			// TWCC feedback has a resolution of 250 us inter packet interval,
			// squash small send intervals getting coalesced on the receiver side.
			// SSBWE-TODO: figure out proper adjustment for measurement resolution, this squelching is not always correct
			delta = 0
		}
		deltas = append(deltas, float64(delta))
		p.logger.Infow("delta", "sn", sn, "send", pi.sendTime, "sendp", piPrev.sendTime, "sd", sendDelta, "r", pi.receiveTime, "rp", piPrev.receiveTime, "rd", receiveDelta, "rawdelta", receiveDelta-sendDelta, "delta", delta) // REMOVE
	}

	p.peakDetector.NextBatch(deltas)
}

func (p *PacketTracker) calculateMPE() {
	p.lock.RLock()
	defer p.lock.RUnlock()

	if !p.ackedInitialized {
		return
	}

	sn := p.highestAckedSN
	endTime := p.packetInfos[sn].receiveTime
	startTime := endTime - 500000 // SSBWE-TODO - make this constant and tune for rate calculation
	// SSBWE-TODO: should this window be dynamic?

	totalError := float64(0.0)
	numDeltas := 0
	for {
		pi := &p.packetInfos[sn]
		piPrev := &p.packetInfos[sn-1]
		if pi.sendTime.IsZero() || piPrev.sendTime.IsZero() {
			break
		}

		// lost packet(s)
		if pi.receiveTime == 0 || piPrev.receiveTime == 0 {
			continue
		}

		// ignore out-of-order arrivals
		if piPrev.receiveTime > pi.receiveTime {
			continue
		}

		if pi.receiveTime < startTime || pi.sendTime.IsZero() {
			break
		}

		sendDelta := pi.sendTime.Sub(piPrev.sendTime).Microseconds()
		receiveDelta := pi.receiveTime - piPrev.receiveTime
		delta := receiveDelta - sendDelta
		if delta < 0 && delta > -rtcp.TypeTCCDeltaScaleFactor {
			// TWCC feedback has a resolution of 250 us inter packet interval,
			// squash small send intervals getting coalesced on the receiver side.
			// SSBWE-TODO: figure out proper adjustment for measurement resolution, this squelching is not always correct
			delta = 0
		}
		// SSBWE-TODO: the error is volatile, need to find a more stable signal - maybe accumulated delay?
		if receiveDelta != 0 {
			totalError += float64(delta) / float64(receiveDelta)
			numDeltas++
			p.logger.Infow("mpe delta", "sn", sn, "send", pi.sendTime, "sendp", piPrev.sendTime, "sd", sendDelta, "r", pi.receiveTime, "rp", piPrev.receiveTime, "rd", receiveDelta, "rawdelta", receiveDelta-sendDelta, "delta", delta, "error", float64(delta)/float64(receiveDelta)) // REMOVE
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
	p.lock.RLock()
	defer p.lock.RUnlock()

	if !p.ackedInitialized {
		return
	}

	sn := p.highestAckedSN
	endTime := p.packetInfos[sn].receiveTime
	startTime := endTime - 500000 // SSBWE-TODO - make this constant and tune for rate calculation
	// SSBWE-TODO: should this window be dynamic?
	// SSBWE-TODO: need to protect against overcalculation when packets arrive too close to each other when congestion is relieving

	highestTime := endTime
	lowestTime := int64(0)
	lowestTimeBytes := 0

	totalBytes := 0
	numPackets := 0
	for {
		pi := &p.packetInfos[sn]
		receiveTime := pi.receiveTime
		if receiveTime == 0 && !pi.sendTime.IsZero() {
			// lost packet or not sent packet
			sn--
			continue
			// SSBWE-TODO think about whether lost packet should be counted for bitrate calculation, probably yes
		}

		if receiveTime > endTime {
			// late arriving (out-of-order arriving) packet
			sn--
			continue
		}

		if receiveTime < startTime || pi.sendTime.IsZero() {
			break
		}

		if receiveTime > highestTime {
			highestTime = receiveTime
		}
		if lowestTime == 0 || receiveTime < lowestTime {
			lowestTime = receiveTime
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

func (p *PacketTracker) worker() {
	for {
		select {
		case <-p.wake:
			for {
				p.lock.Lock()
				if p.feedbackInfos.Len() == 0 {
					p.lock.Unlock()
					break
				}
				fbi := p.feedbackInfos.PopFront()
				p.lock.Unlock()

				p.calculateAcknowledgedBitrate()

				p.detectChangePoint(fbi.baseSN, fbi.baseSN+fbi.numPackets)

				p.calculateMPE()
			}

		case <-p.stop.Watch():
			return
		}
	}
}

// ------------------------------------------------
