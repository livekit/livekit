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
	"github.com/livekit/protocol/utils/mono"
	"github.com/pion/rtcp"
)

// -------------------------------------------------------------------------------

var (
	errNoPacketInRange = errors.New("no packet in range")
)

// -------------------------------------------------------------------------------

type feedbackReport struct {
	at     time.Time
	report *rtcp.TransportLayerCC
}

type PacketTracker struct {
	logger logger.Logger

	twccFeedback *TWCCFeedback

	lock            sync.RWMutex
	sentInitialized bool
	highestSentSN   uint16

	// SSBWE-TODO: make this a ring buffer as a lot more fields are needed
	packetInfos     [2048]packetInfo
	feedbackReports deque.Deque[feedbackReport] // SSBWE-TODO: prune old entries

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
		twccFeedback: NewTWCCFeedback(logger),
		peakDetector: peakdetect.NewPeakDetector(),
		wake:         make(chan struct{}, 1),
		rateCalculator: NewRateCalculator(RateCalculatorParams{
			MeasurementWindow: 500 * time.Millisecond, // SSBWE-TODO: make this config
			Overlap:           0.5,                    // SSBWE-TODO: make this config
			Logger:            logger,
		}),
		estimatedChannelCapacity: 100_000_000,
	}

	// SSBWE-TODO: make consts
	p.peakDetector.Initialize(0.1, 3.5, make([]float64, 60))

	p.feedbackReports.SetMinCapacity(3)

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

// SSBWE-TODO: can this sn operate in extended range for easier comparison?
// SSBWE-TODO: this potentially needs to take isProbe as argument?
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

	pi := &p.packetInfos[int(sn)%len(p.packetInfos)]
	pi.sn = sn
	pi.sendTime = at.UnixMicro()
	pi.headerSize = uint8(headerSize)
	pi.payloadSize = uint16(payloadSize)
	pi.isRTX = isRTX
	pi.ResetReceiveAndDeltas()

	p.highestSentSN = sn
}

func (p *PacketTracker) HandleRTCP(report *rtcp.TransportLayerCC) {
	p.lock.Lock()
	defer p.lock.Unlock()

	p.feedbackReports.PushBack(feedbackReport{mono.Now(), report})

	// notify worker of a new feedback
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *PacketTracker) processFeedbackReport(fbr feedbackReport) (uint16, uint16) {
	p.lock.Lock()
	defer p.lock.Unlock()

	report, err := p.twccFeedback.GetReport(fbr.report, fbr.at)
	if err != nil {
		return 0, 0
	}

	now := mono.UnixMicro()
	if p.debugFile != nil {
		toWrite := fmt.Sprintf("REPORT: start: %d", now)
		p.debugFile.WriteString(toWrite)
		p.debugFile.WriteString("\n")
	}

	if p.activePacketGroup == nil {
		// SSBWE-TODO - spread should be a config option
		p.activePacketGroup = NewPacketGroup(PacketGroupParams{Spread: 50 * time.Millisecond})
	}

	toInt := func(a bool) int {
		if a {
			return 1
		}

		return 0
	}

	updatePacketInfo := func(sn uint16, recvTime int64) {
		pi := p.getPacketInfoExisting(sn)
		if pi == nil {
			return
		}

		pi.receiveTime = recvTime
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
				recvTime,
			)
			p.debugFile.WriteString(toWrite)
			p.debugFile.WriteString("\n")
		}
	}

	sn := report.BaseSequenceNumber
	deltaIdx := 0
	recvRefTime := int64(report.ReferenceTime) * 64 * 1000 // in us
	for _, chunk := range report.PacketChunks {
		switch chunk := chunk.(type) {
		case *rtcp.RunLengthChunk:
			for i := uint16(0); i < chunk.RunLength; i++ {
				if chunk.PacketStatusSymbol != rtcp.TypeTCCPacketNotReceived {
					recvRefTime += report.RecvDeltas[deltaIdx].Delta
					deltaIdx++

					updatePacketInfo(sn, recvRefTime)
				}
				sn++
			}

		case *rtcp.StatusVectorChunk:
			for _, symbol := range chunk.SymbolList {
				if symbol != rtcp.TypeTCCPacketNotReceived {
					recvRefTime += report.RecvDeltas[deltaIdx].Delta
					deltaIdx++

					updatePacketInfo(sn, recvRefTime)
				}
				sn++
			}
		}
	}

	if p.debugFile != nil {
		toWrite := fmt.Sprintf("REPORT: end: %d", now)
		p.debugFile.WriteString(toWrite)
		p.debugFile.WriteString("\n")
	}

	return report.BaseSequenceNumber, report.BaseSequenceNumber + report.PacketStatusCount
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
			// SSBWE-TODO: should this not be ignored?
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

func (p *PacketTracker) getPacketInfoExisting(sn uint16) *packetInfo {
	pi := p.packetInfos[int(sn)%len(p.packetInfos)]
	if pi.sn == sn {
		return &pi
	}

	return nil
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
				if p.feedbackReports.Len() == 0 {
					p.lock.Unlock()
					break
				}
				fbReport := p.feedbackReports.PopFront()
				p.lock.Unlock()

				if startSNInclusive, endSNExclusive := p.processFeedbackReport(fbReport); startSNInclusive != endSNExclusive {
					p.populateDeltas(startSNInclusive, endSNExclusive)

					p.calculateAcknowledgedBitrate(startSNInclusive, endSNExclusive)
					p.rateCalculator.Update(p.packetInfos[:], startSNInclusive, endSNExclusive)

					p.detectChangePoint(startSNInclusive, endSNExclusive)

					p.calculateMPE(startSNInclusive, endSNExclusive)
				}
			}

		case <-p.stop.Watch():
			return
		}
	}
}

// ------------------------------------------------
