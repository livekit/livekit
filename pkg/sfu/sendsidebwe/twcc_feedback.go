package sendsidebwe

import (
	"errors"
	"time"

	"github.com/livekit/protocol/logger"
	"github.com/pion/rtcp"
)

// ------------------------------------------------------

const (
	cOutlierReportFactor            = 3
	cEstimatedFeedbackIntervalAlpha = float64(0.9)

	cReferenceTimeMask       = (1 << 24) - 1
	cReferenceTimeResolution = 64 // 64 ms
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

	highestFeedbackCount uint8

	cycles               int64
	highestReferenceTime uint32
	// SSBWE-TODO- maybe store some history of reports as is, maybe for debugging?
}

func NewTWCCFeedback(params TWCCFeedbackParams) *TWCCFeedback {
	return &TWCCFeedback{
		params: params,
	}
}

func (t *TWCCFeedback) GetReport(report *rtcp.TransportLayerCC, at time.Time) (*rtcp.TransportLayerCC, int64, error) {
	referenceTime, err := t.updateFeedbackState(report, at)
	if err != nil {
		return nil, 0, err
	}

	// REMOVE t.params.Logger.Infow("TWCC feedback", "report", report.String()) // REMOVE
	return report, referenceTime, nil
}

func (t *TWCCFeedback) updateFeedbackState(report *rtcp.TransportLayerCC, at time.Time) (int64, error) {
	if t.lastFeedbackTime.IsZero() {
		t.lastFeedbackTime = at
		t.highestReferenceTime = report.ReferenceTime
		t.highestFeedbackCount = report.FbPktCount
		return (t.cycles + int64(report.ReferenceTime)) * cReferenceTimeResolution * 1000, nil
	}

	if (report.FbPktCount - t.highestFeedbackCount) > (1 << 7) {
		// SSBWE-TODO: should out-of-order reports be dropped or processed??
		return 0, errFeedbackReportOutOfOrder
	}

	// reference time wrap around handling
	var referenceTime int64
	if (report.ReferenceTime-t.highestReferenceTime)&cReferenceTimeMask < (1 << 23) {
		if report.ReferenceTime < t.highestReferenceTime {
			t.cycles += (1 << 24)
		}
		t.highestReferenceTime = report.ReferenceTime
		referenceTime = t.cycles + int64(report.ReferenceTime)
	} else {
		cycles := t.cycles
		if report.ReferenceTime > t.highestReferenceTime && cycles >= (1<<24) {
			cycles -= (1 << 24)
		}
		referenceTime = cycles + int64(report.ReferenceTime)
	}

	sinceLast := at.Sub(t.lastFeedbackTime)
	// REMOVE t.params.Logger.Infow("report received", "at", at, "sinceLast", sinceLast, "pktCount", report.FbPktCount) // REMOVE
	if t.estimatedFeedbackInterval == 0 {
		t.estimatedFeedbackInterval = sinceLast
	} else {
		// filter out outliers from estimate
		if sinceLast > t.estimatedFeedbackInterval/cOutlierReportFactor && sinceLast < cOutlierReportFactor*t.estimatedFeedbackInterval {
			// smoothed version of inter feedback interval
			t.estimatedFeedbackInterval = time.Duration(cEstimatedFeedbackIntervalAlpha*float64(t.estimatedFeedbackInterval) + (1.0-cEstimatedFeedbackIntervalAlpha)*float64(sinceLast))
		}
	}
	t.lastFeedbackTime = at
	t.highestFeedbackCount = report.FbPktCount
	return referenceTime * cReferenceTimeResolution * 1000, nil
}

// ------------------------------------------------
