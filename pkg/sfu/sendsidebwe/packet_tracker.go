package sendsidebwe

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/MicahParks/peakdetect"
	"github.com/frostbyte73/core"
	"github.com/gammazero/deque"
	"github.com/livekit/protocol/logger"
	"github.com/pion/rtcp"
)

// -------------------------------------------------------------------------------

var (
	errNoPacketInRange = errors.New("no packet in range")
)

// -------------------------------------------------------------------------------

type PacketTracker struct {
	logger logger.Logger

	lock            sync.RWMutex
	sentInitialized bool
	highestSentSN   uint16

	// SSBWE-TODO: make this a ring buffer as a lot more fields are needed
	packetInfos   [1 << 16]packetInfo
	feedbackInfos deque.Deque[*TWCCFeedbackInfo] // SSBWE-TODO: prune old entries

	packetGroups      []*PacketGroup // SSBWE-TODO - prune packet groups to some recent history
	activePacketGroup *PacketGroup

	peakDetector peakdetect.PeakDetector

	wake chan struct{}
	stop core.Fuse

	rateCalculator *RateCalculator

	estimatedChannelCapacity int64
	congestionState          CongestionState
	onCongestionStateChange  func(congestionState CongestionState, channelCapacity int64)

	debugFile *os.File
}

func NewPacketTracker(logger logger.Logger) *PacketTracker {
	p := &PacketTracker{
		logger:       logger,
		peakDetector: peakdetect.NewPeakDetector(),
		wake:         make(chan struct{}, 1),
		rateCalculator: NewRateCalculator(RateCalculatorParams{
			MeasurementWindow: 500 * time.Millisecond, // RAJA-TODO: make this config
			Overlap:           0.5,                    // RAJA-TODO: make this config
			Logger:            logger,
		}),
		estimatedChannelCapacity: 100_000_000,
	}

	// SSBWE-TODO: make consts
	p.peakDetector.Initialize(0.1, 3.5, make([]float64, 60))

	p.feedbackInfos.SetMinCapacity(3)

	p.debugFile, _ = os.CreateTemp("/tmp", "twcc")

	go p.worker()
	return p
}

func (p *PacketTracker) Stop() {
	p.stop.Once(func() {
		close(p.wake)
		if p.debugFile != nil {
			p.debugFile.Close()
		}
	})
}

func (p *PacketTracker) OnCongestionStateChange(f func(congestionState CongestionState, channelCapacity int64)) {
	p.lock.Lock()
	defer p.lock.Unlock()
	p.onCongestionStateChange = f
}

func (p *PacketTracker) GetCongestionState() CongestionState {
	p.lock.RLock()
	defer p.lock.RUnlock()
	return p.congestionState
}

func (p *PacketTracker) GetEstimatedChannelCapacity() int64 {
	p.lock.RLock()
	defer p.lock.RUnlock()
	return p.estimatedChannelCapacity
}

func (p *PacketTracker) PacketSent(sn uint16, at time.Time, headerSize int, payloadSize int, isRTX bool) {
	p.lock.Lock()
	defer p.lock.Unlock()

	if !p.sentInitialized {
		p.highestSentSN = sn - 1
		p.sentInitialized = true
	}

	// clear slots occupied by missing packets,
	// ideally this should never run as seequence numbers should be generated in order
	// and packets sent in order.
	if (sn - p.highestSentSN) < (1 << 15) {
		for i := p.highestSentSN + 1; i != sn; i++ {
			pi := &p.packetInfos[i]
			pi.Reset(sn)
		}
	}

	pi := &p.packetInfos[sn]
	pi.sn = sn
	pi.sendTime = at.UnixMicro()
	pi.headerSize = headerSize
	pi.payloadSize = payloadSize
	pi.isRTX = isRTX
	pi.ResetReceiveAndDeltas()

	p.highestSentSN = sn
}

func (p *PacketTracker) ProcessFeedback(fbi *TWCCFeedbackInfo) {
	p.lock.Lock()
	defer p.lock.Unlock()

	p.feedbackInfos.PushBack(fbi)

	// notify worker of a new feedback
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *PacketTracker) processFeedback(fbi *TWCCFeedbackInfo) {
	p.lock.Lock()
	defer p.lock.Unlock()

	now := time.Now()
	if p.debugFile != nil {
		toWrite := fmt.Sprintf("REPORT: start: %d", now.UnixMicro())
		p.debugFile.WriteString(toWrite)
		p.debugFile.WriteString("\n")
	}
	toInt := func(a bool) int {
		if a {
			return 1
		}

		return 0
	}
	if p.activePacketGroup == nil {
		// SSBWE-TODO - spread should be a config option
		p.activePacketGroup = NewPacketGroup(PacketGroupParams{Spread: 50 * time.Millisecond})
	}
	for i, arrival := range fbi.Arrivals {
		sn := fbi.BaseSN + uint16(i)
		pi := &p.packetInfos[sn]
		pi.receiveTime = arrival
		if err := p.activePacketGroup.Add(pi); err != nil {
			p.packetGroups = append(p.packetGroups, p.activePacketGroup)
			p.activePacketGroup = NewPacketGroup(PacketGroupParams{Spread: 50 * time.Millisecond})
			p.activePacketGroup.Add(pi)
		}
		if p.debugFile != nil {
			toWrite := fmt.Sprintf(
				"PACKET: sn: %d, headerSize: %d, payloadSize: %d, isRTX: %d, sendTime: %d, receiveTime: %d",
				sn,
				pi.headerSize,
				pi.payloadSize,
				toInt(pi.isRTX),
				pi.sendTime,
				arrival,
			)
			p.debugFile.WriteString(toWrite)
			p.debugFile.WriteString("\n")
		}
	}
	if p.debugFile != nil {
		toWrite := fmt.Sprintf("REPORT: end: %d", now.UnixMicro())
		p.debugFile.WriteString(toWrite)
		p.debugFile.WriteString("\n")
	}
}

func (p *PacketTracker) populateDeltas(startSNInclusive, endSNExclusive uint16) {
	p.lock.RLock()
	defer p.lock.RUnlock()

	for sn := startSNInclusive; sn != endSNExclusive; sn++ {
		pi := &p.packetInfos[sn]
		piPrev := &p.packetInfos[sn-1]
		if pi.sendTime == 0 || piPrev.sendTime == 0 {
			break
		}

		// lost packet(s)
		if pi.receiveTime == 0 || piPrev.receiveTime == 0 {
			continue
		}

		// ignore out-of-order arrivals
		if piPrev.receiveTime > pi.receiveTime {
			// RAJA-TODO: should this not be ignored?
			continue
		}

		pi.sendDelta = pi.sendTime - piPrev.sendTime
		pi.receiveDelta = pi.receiveTime - piPrev.receiveTime
		pi.deltaOfDelta = pi.receiveDelta - pi.sendDelta
		if pi.deltaOfDelta < 0 && pi.deltaOfDelta > -rtcp.TypeTCCDeltaScaleFactor {
			// TWCC feedback has a resolution of 250 us inter packet interval,
			// squash small send intervals getting coalesced on the receiver side.
			// SSBWE-TODO: figure out proper adjustment for measurement resolution, this squelching is not always correct
			pi.deltaOfDelta = 0
		}
		pi.isDeltaValid = true
		p.logger.Infow(
			"delta",
			"sn", sn,
			"send", pi.sendTime,
			"sendp", piPrev.sendTime,
			"sd", pi.sendDelta,
			"r", pi.receiveTime,
			"rp", piPrev.receiveTime,
			"rd", pi.receiveDelta,
			"rawdelta", pi.receiveDelta-pi.sendDelta,
			"delta", pi.deltaOfDelta,
		) // REMOVE
	}
}

func (p *PacketTracker) detectChangePoint(startSNInclusive, endSNExclusive uint16) {
	p.lock.RLock()
	defer p.lock.RUnlock()

	deltas := make([]float64, 0, endSNExclusive-startSNInclusive)
	for sn := startSNInclusive; sn != endSNExclusive; sn++ {
		pi := &p.packetInfos[sn]
		if pi.isDeltaValid {
			deltas = append(deltas, float64(pi.deltaOfDelta))
		}
	}

	p.peakDetector.NextBatch(deltas)
}

func (p *PacketTracker) calculateMPE(startSNInclusive, endSNExclusive uint16) {
	p.lock.RLock()
	defer p.lock.RUnlock()

	startTime, endTime, err := p.getRange(startSNInclusive, endSNExclusive)
	if err != nil {
		return
	}

	sn := endSNExclusive - 1
	totalError := float64(0.0)
	numDeltas := 0
	for {
		pi := &p.packetInfos[sn]
		if (pi.receiveTime != 0 && pi.receiveTime < startTime) || pi.sendTime == 0 {
			break
		}

		if pi.receiveTime > endTime {
			// late arriving (out-of-order arriving) packet
			sn--
			continue
		}

		// SSBWE-TODO: the error is volatile, need to find a more stable signal - maybe accumulated delay?
		if pi.isDeltaValid {
			totalError += float64(pi.deltaOfDelta) / float64(pi.receiveDelta)
			numDeltas++
			p.logger.Infow(
				"mpe delta",
				"sn", sn,
				"rawdelta", pi.receiveDelta-pi.sendDelta,
				"delta", pi.deltaOfDelta,
				"error", float64(pi.deltaOfDelta)/float64(pi.receiveDelta),
			) // REMOVE
		}
		sn--
	}

	if numDeltas == 0 {
		return
	}

	mpe := float64(totalError) / float64(numDeltas) * 100.0
	p.logger.Infow("mpe", "totalError", totalError, "numDeltas", numDeltas, "mpe", mpe) // REMOVE
}

func (p *PacketTracker) calculateAcknowledgedBitrate(startSNInclusive, endSNExclusive uint16) {
	p.lock.RLock()
	defer p.lock.RUnlock()

	startTime, endTime, err := p.getRange(startSNInclusive, endSNExclusive)
	if err != nil {
		return
	}

	// SSBWE-TODO: need to protect against overcalculation when packets arrive too close to each other when congestion is relieving
	sn := endSNExclusive - 1
	highestTime := endTime
	lowestTime := int64(0)
	lowestTimeBytes := 0
	lowestTimeIsRTX := false

	totalBytes := 0
	numPackets := 0
	totalRTXBytes := 0
	numRTXPackets := 0
	for {
		pi := &p.packetInfos[sn]
		receiveTime := pi.receiveTime
		if receiveTime == 0 && pi.sendTime != 0 {
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

		if receiveTime < startTime || pi.sendTime == 0 {
			break
		}

		if receiveTime > highestTime {
			highestTime = receiveTime
		}
		if lowestTime == 0 || receiveTime < lowestTime {
			lowestTime = receiveTime
			lowestTimeBytes = int(pi.headerSize) + int(pi.payloadSize)
			lowestTimeIsRTX = pi.isRTX
		}

		if pi.isRTX {
			totalRTXBytes += int(pi.headerSize) + int(pi.payloadSize)
			numRTXPackets++
		} else {
			totalBytes += int(pi.headerSize) + int(pi.payloadSize)
			numPackets++
		}
		sn--
	}

	interval := float64(highestTime-lowestTime) / 1e6
	if interval == 0 || numPackets < 2 {
		return
	}

	// take out the edge
	if lowestTimeIsRTX {
		numRTXPackets--
		totalRTXBytes -= lowestTimeBytes
	} else {
		numPackets--
		totalBytes -= lowestTimeBytes
	}

	bitrate := float64(totalBytes) * 8 / interval
	packetRate := float64(numPackets) / interval
	bitrateRTX := float64(totalRTXBytes) * 8 / interval
	packetRateRTX := float64(numRTXPackets) / interval
	p.logger.Infow("bitrate calculation",
		"highest", highestTime,
		"lowest", lowestTime,
		"interval", interval,
		"totalBytes", totalBytes,
		"bitrate", bitrate,
		"numPackets", numPackets,
		"packetRate", packetRate,
		"totalRTXBytes", totalRTXBytes,
		"bitrateRTX", bitrateRTX,
		"numRTXPackets", numRTXPackets,
		"packetRateRTX", packetRateRTX,
	) // REMOVE
}

func (p *PacketTracker) getRange(startSNInclusive, endSNExclusive uint16) (startTime int64, endTime int64, err error) {
	for sn := endSNExclusive - 1; sn != startSNInclusive-1; sn-- {
		pi := &p.packetInfos[sn]
		if pi.receiveTime != 0 {
			endTime = pi.receiveTime
			startTime = endTime - 500000 // SSBWE-TODO - make this constant and tune for rate calculation/other error measurement windows
			// SSBWE-TODO: should this window be dynamic?
			return
		}
	}

	err = errNoPacketInRange
	return
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

				p.processFeedback(fbi)

				startSNInclusive := fbi.BaseSN
				endSNExclusive := fbi.BaseSN + uint16(len(fbi.Arrivals))

				p.populateDeltas(startSNInclusive, endSNExclusive)

				p.calculateAcknowledgedBitrate(startSNInclusive, endSNExclusive)
				p.rateCalculator.Update(p.packetInfos, startSNInclusive, endSNExclusive)

				p.detectChangePoint(startSNInclusive, endSNExclusive)

				p.calculateMPE(startSNInclusive, endSNExclusive)
			}

		case <-p.stop.Watch():
			return
		}
	}
}

// ------------------------------------------------
