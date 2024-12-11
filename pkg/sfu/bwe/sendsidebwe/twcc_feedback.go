// Copyright 2023 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package sendsidebwe

import (
	"errors"
	"time"

	"github.com/livekit/protocol/logger"
	"github.com/pion/rtcp"
	"go.uber.org/zap/zapcore"
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

type twccFeedbackParams struct {
	Logger logger.Logger
}

type twccFeedback struct {
	params twccFeedbackParams

	lastFeedbackTime          time.Time
	estimatedFeedbackInterval time.Duration
	numReports                int
	numReportsOutOfOrder      int

	highestFeedbackCount uint8

	cycles               int64
	highestReferenceTime uint32
}

func newTWCCFeedback(params twccFeedbackParams) *twccFeedback {
	return &twccFeedback{
		params: params,
	}
}

func (t *twccFeedback) ProcessReport(report *rtcp.TransportLayerCC, at time.Time) (int64, bool) {
	t.numReports++
	if t.lastFeedbackTime.IsZero() {
		t.lastFeedbackTime = at
		t.highestReferenceTime = report.ReferenceTime
		t.highestFeedbackCount = report.FbPktCount
		return (t.cycles + int64(report.ReferenceTime)) * cReferenceTimeResolution * 1000, false
	}

	isOutOfOrder := false
	if (report.FbPktCount - t.highestFeedbackCount) > (1 << 7) {
		t.numReportsOutOfOrder++
		isOutOfOrder = true
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

	if !isOutOfOrder {
		sinceLast := at.Sub(t.lastFeedbackTime)
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
	}

	return referenceTime * cReferenceTimeResolution * 1000, isOutOfOrder
}

func (t *twccFeedback) MarshalLogObject(e zapcore.ObjectEncoder) error {
	if t == nil {
		return nil
	}

	e.AddTime("lastFeedbackTime", t.lastFeedbackTime)
	e.AddDuration("estimatedFeedbackInterval", t.estimatedFeedbackInterval)
	e.AddInt("numReports", t.numReports)
	e.AddInt("numReportsOutOfOrder", t.numReportsOutOfOrder)
	e.AddInt64("cycles", t.cycles/(1<<24))
	return nil
}
