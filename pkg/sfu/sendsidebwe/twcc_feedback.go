package sendsidebwe

import (
	"errors"
	"time"

	"github.com/livekit/protocol/logger"
	"github.com/pion/rtcp"
)

// ------------------------------------------------------

const (
	outlierReportFactor            = 4
	estimatedFeedbackIntervalAlpha = float64(0.9)
)

// ------------------------------------------------------

var (
	errFeedbackReportOutOfOrder = errors.New("feedback report out-of-order")
)

// ------------------------------------------------------

type TWCCFeedback struct {
	logger logger.Logger

	lastFeedbackTime          time.Time
	estimatedFeedbackInterval time.Duration
	highestFeedbackCount      uint8
	// SSBWE-TODO- maybe store some history of reports as is, maybe for debugging?
}

func NewTWCCFeedback(logger logger.Logger) *TWCCFeedback {
	return &TWCCFeedback{
		logger: logger,
	}
}

func (t *TWCCFeedback) GetReport(report *rtcp.TransportLayerCC, at time.Time) (*rtcp.TransportLayerCC, error) {
	if err := t.updateFeedbackState(report, at); err != nil {
		return nil, err
	}

	t.logger.Infow("TWCC feedback", "report", report.String()) // REMOVE
	return report, nil
}

func (t *TWCCFeedback) updateFeedbackState(report *rtcp.TransportLayerCC, at time.Time) error {
	if !t.lastFeedbackTime.IsZero() {
		if (report.FbPktCount - t.highestFeedbackCount) < (1 << 7) { // in-order}
			sinceLast := at.Sub(t.lastFeedbackTime)
			if t.estimatedFeedbackInterval == 0 {
				t.estimatedFeedbackInterval = sinceLast
			} else {
				// filter out outliers from estimate
				if sinceLast > t.estimatedFeedbackInterval/outlierReportFactor && sinceLast < outlierReportFactor*t.estimatedFeedbackInterval {
					// smoothed version of inter feedback interval
					t.estimatedFeedbackInterval = time.Duration(estimatedFeedbackIntervalAlpha*float64(t.estimatedFeedbackInterval) + (1.0-estimatedFeedbackIntervalAlpha)*float64(sinceLast))
				}
			}
		} else {
			return errFeedbackReportOutOfOrder
		}
	}

	t.lastFeedbackTime = at
	t.highestFeedbackCount = report.FbPktCount
	return nil
}

// ------------------------------------------------
