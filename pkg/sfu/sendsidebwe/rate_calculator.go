package sendsidebwe

import (
	"fmt"
	"time"

	"github.com/livekit/protocol/logger"
)

// ---------------------------------------------------------------------

type byteCounter struct {
	header  int
	payload int
}

func (b byteCounter) String() string {
	return fmt.Sprintf("h: %d, p: %d", b.header, b.payload)
}

// ---------------------------------------------------------------------

type measurement struct {
	at time.Time

	primary byteCounter
	rtx     byteCounter

	sendStart   int64
	sendEnd     int64
	sendBitrate float64

	recvStart   int64
	recvEnd     int64
	recvBitrate float64
}

func (m measurement) String() string {
	return fmt.Sprintf("at: %s, primary: (%s), rtx: (%s), send: (%d / %d / %.2f /  %.2f), recv: (%d / %d / %.2f / %.2f)",
		m.at.String(),
		m.primary.String(),
		m.rtx.String(),
		m.sendStart, m.sendEnd, float64(m.sendEnd-m.sendStart)/1e6, m.sendBitrate,
		m.recvStart, m.recvEnd, float64(m.recvEnd-m.recvStart)/1e6, m.recvBitrate,
	)
}

// ---------------------------------------------------------------------

type RateCalculatorParams struct {
	MeasurementWindow time.Duration
	Overlap           float64
	// SSBWE-TODO: maybe add a config for how much of window should be available to have a valid rate

	Logger logger.Logger
}

type RateCalculator struct {
	params RateCalculatorParams

	packetInfos               []*packetInfo
	highestAckedSN            uint16
	highestAckedSNInitialized bool
	latestRecvTime            int64
	primary                   byteCounter
	rtx                       byteCounter

	nextMeasurementTime int64
	measurements        []measurement // SSBWE-TODO: need to trim these
}

func NewRateCalculator(params RateCalculatorParams) *RateCalculator {
	return &RateCalculator{
		params: params,
	}
}

func (r *RateCalculator) Update(packetInfos []packetInfo, startSNInclusive, endSNExclusive uint16) {
	r.add(packetInfos, startSNInclusive, endSNExclusive)
	r.prune()
}

func (r *RateCalculator) add(packetInfos []packetInfo, startSNInclusive, endSNExclusive uint16) {
	latestRecvTimeInCluster := int64(0)
	for sn := startSNInclusive; sn != endSNExclusive; sn++ {
		pi := &packetInfos[int(sn)%len(packetInfos)]
		if pi.recvTime == 0 {
			// potentially lost packet, may arrive later
			continue
		}

		if r.nextMeasurementTime == 0 {
			// first one maybe a short window if overlap is non-zero
			r.nextMeasurementTime = pi.recvTime + int64((1.0-r.params.Overlap)*float64(r.params.MeasurementWindow.Microseconds()))
		}
		if !r.highestAckedSNInitialized {
			r.highestAckedSNInitialized = true
			r.highestAckedSN = pi.sn - 1
		}

		// add to history if received in-order OR received out-of-order, but received after last cluster
		diff := pi.sn - r.highestAckedSN
		if diff < (1<<15) || pi.recvTime >= r.latestRecvTime {
			r.packetInfos = append(r.packetInfos, pi)
		}
		if diff < (1 << 15) {
			r.highestAckedSN = pi.sn
		}
		if latestRecvTimeInCluster == 0 || pi.recvTime > latestRecvTimeInCluster {
			latestRecvTimeInCluster = pi.recvTime
		}

		if pi.isRTX {
			r.rtx.header += int(pi.headerSize)
			r.rtx.payload += int(pi.payloadSize)
		} else {
			r.primary.header += int(pi.headerSize)
			r.primary.payload += int(pi.payloadSize)
		}

		if pi.recvTime >= r.nextMeasurementTime {
			r.prune()
			r.calculate()
			r.nextMeasurementTime += int64((1.0 - r.params.Overlap) * float64(r.params.MeasurementWindow.Microseconds()))
		}
	}
	if latestRecvTimeInCluster != 0 && latestRecvTimeInCluster > r.latestRecvTime {
		r.latestRecvTime = latestRecvTimeInCluster
	}
}

func (r *RateCalculator) prune() {
	if len(r.packetInfos) == 0 {
		return
	}

	cutoffTime := r.packetInfos[len(r.packetInfos)-1].recvTime - r.params.MeasurementWindow.Microseconds()
	cutoffIdx := 0
	for idx, pi := range r.packetInfos {
		if pi.recvTime >= cutoffTime {
			cutoffIdx = idx
			break
		}

		if pi.isRTX {
			r.rtx.header -= int(pi.headerSize)
			r.rtx.payload -= int(pi.payloadSize)
		} else {
			r.primary.header -= int(pi.headerSize)
			r.primary.payload -= int(pi.payloadSize)
		}
	}

	r.packetInfos = r.packetInfos[cutoffIdx:]
}

func (r *RateCalculator) calculate() {
	first := r.packetInfos[0]
	sendStart := first.sendTime
	sendEnd := r.packetInfos[len(r.packetInfos)-1].sendTime
	sendDuration := float64(sendEnd-sendStart) / 1e6
	recvStart := first.recvTime
	recvEnd := r.packetInfos[len(r.packetInfos)-1].recvTime
	recvDuration := float64(recvEnd-recvStart) / 1e6
	m := measurement{
		at:        time.Now(),
		sendStart: sendStart,
		sendEnd:   sendEnd,
		recvStart: recvStart,
		recvEnd:   recvEnd,
	}
	if first.isRTX {
		m.rtx = byteCounter{
			header:  r.rtx.header - int(first.headerSize),
			payload: r.rtx.payload - int(first.payloadSize),
		}
	} else {
		m.primary = byteCounter{
			header:  r.primary.header - int(first.headerSize),
			payload: r.primary.payload - int(first.payloadSize),
		}
	}
	m.sendBitrate = float64((m.primary.header+m.primary.payload+m.rtx.header+m.rtx.payload)*8) / sendDuration
	m.recvBitrate = float64((m.primary.header+m.primary.payload+m.rtx.header+m.rtx.payload)*8) / recvDuration
	r.params.Logger.Debugw("rate calculator measurement", "measurement", m.String()) // REMOVE
	r.measurements = append(r.measurements, m)
}
