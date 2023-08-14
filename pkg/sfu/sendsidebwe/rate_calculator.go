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
	at           time.Time
	receiveStart int64
	receiveEnd   int64
	primary      byteCounter
	rtx          byteCounter
	bitrate      float64
}

func (m measurement) String() string {
	return fmt.Sprintf("at: %s, span: %d/%d/%.2f, primary: (%s), rtx: (%s), bitrate: %.2f",
		m.at.String(),
		m.receiveStart, m.receiveEnd, float64(m.receiveEnd-m.receiveStart)/1e6,
		m.primary.String(),
		m.rtx.String(),
		m.bitrate,
	)
}

// ---------------------------------------------------------------------

type RateCalculatorParams struct {
	MeasurementWindow time.Duration
	Overlap           float64
	// RAJA-TODO: maybe add a config for how much of window should be available to have a valid rate

	Logger logger.Logger
}

type RateCalculator struct {
	params RateCalculatorParams

	packetInfos               []*packetInfo
	highestAckedSN            uint16
	highestAckedSNInitialized bool
	latestReceiveTime         int64
	primary                   byteCounter
	rtx                       byteCounter

	nextMeasurementTime int64
	measurements        []measurement // RAJA-TODO: need to trim these
}

func NewRateCalculator(params RateCalculatorParams) *RateCalculator {
	return &RateCalculator{
		params: params,
	}
}

func (r *RateCalculator) Update(packetInfos [1 << 16]packetInfo, startSNInclusive, endSNExclusive uint16) {
	r.add(packetInfos, startSNInclusive, endSNExclusive)
	r.prune()
}

func (r *RateCalculator) add(packetInfos [1 << 16]packetInfo, startSNInclusive, endSNExclusive uint16) {
	latestReceiveTimeInCluster := int64(0)
	for sn := startSNInclusive; sn != endSNExclusive; sn++ {
		pi := &packetInfos[sn]
		if pi.receiveTime == 0 {
			// potentially lost packet, may arrive later
			continue
		}

		if r.nextMeasurementTime == 0 {
			// first one maybe a short window if overlap is non-zero
			r.nextMeasurementTime = pi.receiveTime + int64((1.0-r.params.Overlap)*float64(r.params.MeasurementWindow.Microseconds()))
		}
		if !r.highestAckedSNInitialized {
			r.highestAckedSNInitialized = true
			r.highestAckedSN = pi.sn - 1
		}

		// add to history if received in-order OR received out-of-order, but received after last cluster
		diff := pi.sn - r.highestAckedSN
		if diff < (1<<15) || pi.receiveTime >= r.latestReceiveTime {
			r.packetInfos = append(r.packetInfos, pi)
		}
		if diff < (1 << 15) {
			r.highestAckedSN = pi.sn
		}
		if latestReceiveTimeInCluster == 0 || pi.receiveTime > latestReceiveTimeInCluster {
			latestReceiveTimeInCluster = pi.receiveTime
		}

		if pi.isRTX {
			r.rtx.header += pi.headerSize
			r.rtx.payload += pi.payloadSize
		} else {
			r.primary.header += pi.headerSize
			r.primary.payload += pi.payloadSize
		}

		if pi.receiveTime >= r.nextMeasurementTime {
			r.prune()
			r.calculate()
			r.nextMeasurementTime += r.params.MeasurementWindow.Microseconds()
		}
	}
	if latestReceiveTimeInCluster != 0 && latestReceiveTimeInCluster > r.latestReceiveTime {
		r.latestReceiveTime = latestReceiveTimeInCluster
	}
}

func (r *RateCalculator) prune() {
	if len(r.packetInfos) == 0 {
		return
	}

	cutoffTime := r.packetInfos[len(r.packetInfos)-1].receiveTime - r.params.MeasurementWindow.Microseconds()
	cutoffIdx := 0
	for idx, pi := range r.packetInfos {
		if pi.receiveTime >= cutoffTime {
			cutoffIdx = idx
			break
		}

		if pi.isRTX {
			r.rtx.header -= pi.headerSize
			r.rtx.payload -= pi.payloadSize
		} else {
			r.primary.header -= pi.headerSize
			r.primary.payload -= pi.payloadSize
		}
	}

	r.packetInfos = r.packetInfos[cutoffIdx:]
}

func (r *RateCalculator) calculate() {
	first := r.packetInfos[0]
	receiveStart := first.receiveTime
	receiveEnd := r.packetInfos[len(r.packetInfos)-1].receiveTime
	duration := float64(receiveEnd-receiveStart) / float64(time.Microsecond)
	m := measurement{
		at:           time.Now(),
		receiveStart: receiveStart,
		receiveEnd:   receiveEnd,
	}
	if first.isRTX {
		m.rtx = byteCounter{
			header:  r.rtx.header - first.headerSize,
			payload: r.rtx.payload - first.payloadSize,
		}
	} else {
		m.primary = byteCounter{
			header:  r.primary.header - first.headerSize,
			payload: r.primary.payload - first.payloadSize,
		}
	}
	m.bitrate = float64((m.primary.header+m.primary.payload+m.rtx.header+m.rtx.payload)*8) * float64(time.Second) / duration
	r.params.Logger.Debugw("rate calculator measurement", "measurement", m.String()) // REMOVE
	r.measurements = append(r.measurements, m)
}
