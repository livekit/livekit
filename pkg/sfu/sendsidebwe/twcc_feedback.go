package sendsidebwe

import (
	"errors"
	"sync"
	"time"

	"github.com/livekit/protocol/logger"
	"github.com/pion/rtcp"
)

const (
	outlierReportFactor            = 4
	estimatedFeedbackIntervalAlpha = float64(0.9)
)

var (
	errFeedbackReportOutOfOrder = errors.New("feedback report out-of-order")
)

type TWCCFeedback struct {
	logger logger.Logger

	lock                      sync.RWMutex
	lastFeedbackTime          time.Time
	estimatedFeedbackInterval time.Duration
	highestFeedbackCount      uint8
	// RAJA-TODO- maybe just store some history of reports as is
}

func NewTWCCFeedback(logger logger.Logger) *TWCCFeedback {
	return &TWCCFeedback{
		logger: logger,
	}
}

func (t *TWCCFeedback) HandleRTCP(report *rtcp.TransportLayerCC) (uint16, []int64, error) {
	t.lock.Lock()
	defer t.lock.Unlock()

	sinceLast := time.Duration(0)
	isInOrder := true
	now := time.Now()
	if !t.lastFeedbackTime.IsZero() {
		isInOrder = (report.FbPktCount - t.highestFeedbackCount) < (1 << 7)
		if isInOrder {
			sinceLast = now.Sub(t.lastFeedbackTime)
			if t.estimatedFeedbackInterval == 0 {
				t.estimatedFeedbackInterval = sinceLast
			} else {
				// filter out outliers from estimate
				if sinceLast > t.estimatedFeedbackInterval/outlierReportFactor && sinceLast < outlierReportFactor*t.estimatedFeedbackInterval {
					// smoothed version of inter feedback interval
					t.estimatedFeedbackInterval = time.Duration(estimatedFeedbackIntervalAlpha*float64(t.estimatedFeedbackInterval) + (1.0-estimatedFeedbackIntervalAlpha)*float64(sinceLast))
				}
			}
		}
	}
	if !isInOrder {
		return 0, nil, errFeedbackReportOutOfOrder
	}

	t.lastFeedbackTime = now
	t.highestFeedbackCount = report.FbPktCount

	arrivals := make([]int64, report.PacketStatusCount)
	snIdx := 0
	deltaIdx := 0
	refTime := int64(report.ReferenceTime) * 64 * 1000 // in us
	for _, chunk := range report.PacketChunks {
		switch chunk := chunk.(type) {
		case *rtcp.RunLengthChunk:
			for i := uint16(0); i < chunk.RunLength; i++ {
				if chunk.PacketStatusSymbol != rtcp.TypeTCCPacketNotReceived {
					refTime += report.RecvDeltas[deltaIdx].Delta
					deltaIdx++

					arrivals[snIdx] = refTime
				}
				snIdx++
			}

		case *rtcp.StatusVectorChunk:
			for _, symbol := range chunk.SymbolList {
				if symbol != rtcp.TypeTCCPacketNotReceived {
					refTime += report.RecvDeltas[deltaIdx].Delta
					deltaIdx++

					arrivals[snIdx] = refTime
				}
				snIdx++
			}
		}
	}

	return report.BaseSequenceNumber, arrivals, nil
}

// ------------------------------------------------
