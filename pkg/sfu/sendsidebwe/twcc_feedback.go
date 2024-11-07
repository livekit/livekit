package sendsidebwe

import (
	"errors"
	"time"

	"github.com/livekit/protocol/logger"
	"github.com/pion/rtcp"
)

// ------------------------------------------------------

const (
	outlierReportFactor            = 3
	estimatedFeedbackIntervalAlpha = float64(0.9)
)

// ------------------------------------------------------

var (
	errFeedbackReportOutOfOrder = errors.New("feedback report out-of-order")
)

// ------------------------------------------------------

type TWCCFeedbackParams struct {
	Logger logger.Logger
}

// SSBWE-TODO: what is the purpose of this module? is it needed?
// maybe, it needs to flag large gaps between reports?
type TWCCFeedback struct {
	params TWCCFeedbackParams

	lastFeedbackTime          time.Time
	estimatedFeedbackInterval time.Duration
	highestFeedbackCount      uint8
	// SSBWE-TODO- maybe store some history of reports as is, maybe for debugging?
}

func NewTWCCFeedback(params TWCCFeedbackParams) *TWCCFeedback {
	return &TWCCFeedback{
		params: params,
	}
}

func (t *TWCCFeedback) GetReport(report *rtcp.TransportLayerCC, at time.Time) (*rtcp.TransportLayerCC, error) {
	if err := t.updateFeedbackState(report, at); err != nil {
		return nil, err
	}

	// REMOVE t.params.Logger.Infow("TWCC feedback", "report", report.String()) // REMOVE
	return report, nil
}

func (t *TWCCFeedback) updateFeedbackState(report *rtcp.TransportLayerCC, at time.Time) error {
	if !t.lastFeedbackTime.IsZero() {
		if (report.FbPktCount - t.highestFeedbackCount) < (1 << 7) { // in-order}
			sinceLast := at.Sub(t.lastFeedbackTime)
			// REMOVE t.params.Logger.Infow("report received", "at", at, "sinceLast", sinceLast, "pktCount", report.FbPktCount) // REMOVE
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
