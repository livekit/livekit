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

package buffer

import (
	"fmt"
	"math"
	"time"

	"github.com/pion/rtcp"
	"go.uber.org/zap/zapcore"

	"github.com/livekit/livekit-server/pkg/sfu/utils"
	"github.com/livekit/protocol/livekit"
	protoutils "github.com/livekit/protocol/utils"
)

const (
	cHistorySize = 8192

	// number of seconds the current report RTP timestamp can be off from expected RTP timestamp
	cReportSlack = float64(60.0)

	cTSJumpTooHighFactor = float64(1.5)
)

// ---------------------------------------------------------------------

type RTPFlowState struct {
	IsNotHandled bool

	HasLoss            bool
	LossStartInclusive uint64
	LossEndExclusive   uint64

	IsDuplicate  bool
	IsOutOfOrder bool

	ExtSequenceNumber uint64
	ExtTimestamp      uint64
}

func (r *RTPFlowState) MarshalLogObject(e zapcore.ObjectEncoder) error {
	if r == nil {
		return nil
	}

	e.AddBool("IsNotHandled", r.IsNotHandled)
	e.AddBool("HasLoss", r.HasLoss)
	e.AddUint64("LossStartInclusive", r.LossStartInclusive)
	e.AddUint64("LossEndExclusive", r.LossEndExclusive)
	e.AddBool("IsDuplicate", r.IsDuplicate)
	e.AddBool("IsOutOfOrder", r.IsOutOfOrder)
	e.AddUint64("ExtSequenceNumber", r.ExtSequenceNumber)
	e.AddUint64("ExtTimestamp", r.ExtTimestamp)
	return nil
}

// ---------------------------------------------------------------------

type RTPStatsReceiver struct {
	*rtpStatsBase

	sequenceNumber *utils.WrapAround[uint16, uint64]

	tsRolloverThreshold int64
	timestamp           *utils.WrapAround[uint32, uint64]

	history *protoutils.Bitmap[uint64]

	propagationDelayEstimator *utils.OWDEstimator

	clockSkewCount              int
	clockSkewMediaPathCount     int
	outOfOrderSenderReportCount int
	largeJumpCount              int
	largeJumpNegativeCount      int
	timeReversedCount           int
}

func NewRTPStatsReceiver(params RTPStatsParams) *RTPStatsReceiver {
	return &RTPStatsReceiver{
		rtpStatsBase:              newRTPStatsBase(params),
		sequenceNumber:            utils.NewWrapAround[uint16, uint64](utils.WrapAroundParams{IsRestartAllowed: false}),
		tsRolloverThreshold:       (1 << 31) * 1e9 / int64(params.ClockRate),
		timestamp:                 utils.NewWrapAround[uint32, uint64](utils.WrapAroundParams{IsRestartAllowed: false}),
		history:                   protoutils.NewBitmap[uint64](cHistorySize),
		propagationDelayEstimator: utils.NewOWDEstimator(utils.OWDEstimatorParamsDefault),
	}
}

func (r *RTPStatsReceiver) NewSnapshotId() uint32 {
	r.lock.Lock()
	defer r.lock.Unlock()

	return r.newSnapshotID(r.sequenceNumber.GetExtendedHighest())
}

func (r *RTPStatsReceiver) getTSRolloverCount(diffNano int64, ts uint32) int {
	if diffNano < r.tsRolloverThreshold {
		// time not more than rollover threshold
		return -1
	}

	excess := (diffNano - r.tsRolloverThreshold*2) * int64(r.params.ClockRate) / 1e9
	roc := excess / (1 << 32)
	if roc < 0 {
		roc = 0
	}
	if r.timestamp.GetHighest() > ts {
		roc++
	}
	return int(roc)
}

func (r *RTPStatsReceiver) Update(
	packetTime int64,
	sequenceNumber uint16,
	timestamp uint32,
	marker bool,
	hdrSize int,
	payloadSize int,
	paddingSize int,
) (flowState RTPFlowState) {
	r.lock.Lock()
	defer r.lock.Unlock()

	if !r.endTime.IsZero() {
		flowState.IsNotHandled = true
		return
	}

	var resSN utils.WrapAroundUpdateResult[uint64]
	var gapSN int64
	var resTS utils.WrapAroundUpdateResult[uint64]
	var timeSinceHighest int64
	var expectedTSJump int64
	var tsRolloverCount int
	var snRolloverCount int

	getLoggingFields := func() []interface{} {
		return []interface{}{
			"resSN", resSN,
			"gapSN", gapSN,
			"resTS", resTS,
			"gapTS", int64(resTS.ExtendedVal - resTS.PreExtendedHighest),
			"timeSinceHighest", time.Duration(timeSinceHighest),
			"snRolloverCount", snRolloverCount,
			"expectedTSJump", expectedTSJump,
			"tsRolloverCount", tsRolloverCount,
			"packetTime", time.Unix(0, packetTime).String(),
			"sequenceNumber", sequenceNumber,
			"timestamp", timestamp,
			"marker", marker,
			"hdrSize", hdrSize,
			"payloadSize", payloadSize,
			"paddingSize", paddingSize,
			"rtpStats", lockedRTPStatsReceiverLogEncoder{r},
		}
	}

	if !r.initialized {
		if payloadSize == 0 {
			// do not start on a padding only packet
			flowState.IsNotHandled = true
			return
		}

		r.initialized = true

		r.startTime = time.Now()

		r.firstTime = packetTime
		r.highestTime = packetTime

		resSN = r.sequenceNumber.Update(sequenceNumber)
		resTS = r.timestamp.Update(timestamp)

		// initialize snapshots if any
		for i := uint32(0); i < r.nextSnapshotID-cFirstSnapshotID; i++ {
			r.snapshots[i] = r.initSnapshot(r.startTime, r.sequenceNumber.GetExtendedStart())
		}

		r.logger.Debugw(
			"rtp receiver stream start",
			"rtpStats", lockedRTPStatsReceiverLogEncoder{r},
		)
	} else {
		resSN = r.sequenceNumber.Update(sequenceNumber)
		if resSN.IsUnhandled {
			flowState.IsNotHandled = true
			return
		}
		gapSN = int64(resSN.ExtendedVal - resSN.PreExtendedHighest)

		timeSinceHighest = packetTime - r.highestTime
		tsRolloverCount = r.getTSRolloverCount(timeSinceHighest, timestamp)
		if tsRolloverCount >= 0 {
			r.logger.Warnw(
				"potential time stamp roll over", nil,
				getLoggingFields()...,
			)
		}
		resTS = r.timestamp.Rollover(timestamp, tsRolloverCount)
		if resTS.IsUnhandled {
			flowState.IsNotHandled = true
			return
		}
		gapTS := int64(resTS.ExtendedVal - resTS.PreExtendedHighest)

		// it is possible to reecive old packets in two different scenarios
		// as it is not possible to detect how far to roll back, ignore old packets
		//
		// Case 1:
		//  Very old time stamp, happens under the following conditions
		//  - resume after long mute, big time stamp jump
		//  - an out of order packet from before the mute arrives (unsure what causes this
		//    very old packet to be trasmitted from remote), causing time stamp to jump back
		//    to before mute, but it appears like it has rolled over.
		//  Use a threshold against expected to ignore these.
		if gapSN < 0 && gapTS > 0 {
			expectedTSJump = timeSinceHighest * int64(r.params.ClockRate) / 1e9
			if gapTS > int64(float64(expectedTSJump)*cTSJumpTooHighFactor) {
				r.sequenceNumber.UndoUpdate(resSN)
				r.timestamp.UndoUpdate(resTS)
				r.logger.Warnw(
					"dropping old packet, timestamp", nil,
					getLoggingFields()...,
				)
				flowState.IsNotHandled = true
				return
			}
		}

		// Case 2:
		//  Sequence number looks like it is moving forward, but it is actually a very old packet.
		if gapTS < 0 && gapSN > 0 {
			r.sequenceNumber.UndoUpdate(resSN)
			r.timestamp.UndoUpdate(resTS)
			r.logger.Warnw(
				"dropping old packet, sequence number", nil,
				getLoggingFields()...,
			)
			flowState.IsNotHandled = true
			return
		}

		// it is possible that sequence number has rolled over too
		if gapSN < 0 && gapTS > 0 && payloadSize > 0 {
			// not possible to know how many cycles of sequence number roll over could have happened,
			// ensure that it at least does not go backwards
			snRolloverCount = 0
			if sequenceNumber < r.sequenceNumber.GetHighest() {
				snRolloverCount = 1
			}
			resSN = r.sequenceNumber.Rollover(sequenceNumber, snRolloverCount)
			if resSN.IsUnhandled {
				flowState.IsNotHandled = true
				return
			}

			r.logger.Warnw(
				"forcing sequence number rollover", nil,
				getLoggingFields()...,
			)
		}
	}
	gapSN = int64(resSN.ExtendedVal - resSN.PreExtendedHighest)

	pktSize := uint64(hdrSize + payloadSize + paddingSize)
	if gapSN <= 0 { // duplicate OR out-of-order
		if gapSN != 0 {
			r.packetsOutOfOrder++
		}

		if r.isInRange(resSN.ExtendedVal, resSN.PreExtendedHighest) {
			if r.history.IsSet(resSN.ExtendedVal) {
				r.bytesDuplicate += pktSize
				r.headerBytesDuplicate += uint64(hdrSize)
				r.packetsDuplicate++
				flowState.IsDuplicate = true
			} else {
				r.packetsLost--
				r.history.Set(resSN.ExtendedVal)
			}
		}

		flowState.IsOutOfOrder = true

		if !flowState.IsDuplicate && -gapSN >= cSequenceNumberLargeJumpThreshold {
			r.largeJumpNegativeCount++
			if (r.largeJumpNegativeCount-1)%100 == 0 {
				r.logger.Warnw(
					"large sequence number gap negative", nil,
					append(getLoggingFields(), "count", r.largeJumpNegativeCount)...,
				)
			}
		}
	} else { // in-order
		if gapSN >= cSequenceNumberLargeJumpThreshold {
			r.largeJumpCount++
			if (r.largeJumpCount-1)%100 == 0 {
				r.logger.Warnw(
					"large sequence number gap", nil,
					append(getLoggingFields(), "count", r.largeJumpCount)...,
				)
			}
		}

		if resTS.ExtendedVal < resTS.PreExtendedHighest {
			r.timeReversedCount++
			if (r.timeReversedCount-1)%100 == 0 {
				r.logger.Warnw(
					"time reversed", nil,
					append(getLoggingFields(), "count", r.timeReversedCount)...,
				)
			}
		}

		// update gap histogram
		r.updateGapHistogram(int(gapSN))

		// update missing sequence numbers
		r.history.ClearRange(resSN.PreExtendedHighest+1, resSN.ExtendedVal-1)
		r.packetsLost += uint64(gapSN - 1)

		r.history.Set(resSN.ExtendedVal)

		if timestamp != uint32(resTS.PreExtendedHighest) {
			// update only on first packet as same timestamp could be in multiple packets.
			// NOTE: this may not be the first packet with this time stamp if there is packet loss.
			r.highestTime = packetTime
		}

		if gapSN > 1 {
			flowState.HasLoss = true
			flowState.LossStartInclusive = resSN.PreExtendedHighest + 1
			flowState.LossEndExclusive = resSN.ExtendedVal
		}
	}
	flowState.ExtSequenceNumber = resSN.ExtendedVal
	flowState.ExtTimestamp = resTS.ExtendedVal

	if !flowState.IsDuplicate {
		if payloadSize == 0 {
			r.packetsPadding++
			r.bytesPadding += pktSize
			r.headerBytesPadding += uint64(hdrSize)
		} else {
			r.bytes += pktSize
			r.headerBytes += uint64(hdrSize)

			if marker {
				r.frames++
			}

			r.updateJitter(resTS.ExtendedVal, packetTime)
		}
	}
	return
}

func (r *RTPStatsReceiver) getExtendedSenderReport(srData *RTCPSenderReportData) *RTCPSenderReportData {
	tsCycles := uint64(0)
	if r.srNewest != nil {
		// use time since last sender report to ensure long gaps where the time stamp might
		// jump more than half the range
		timeSinceLastReport := srData.NTPTimestamp.Time().Sub(r.srNewest.NTPTimestamp.Time())
		expectedRTPTimestampExt := r.srNewest.RTPTimestampExt + uint64(timeSinceLastReport.Nanoseconds()*int64(r.params.ClockRate)/1e9)
		lbound := expectedRTPTimestampExt - uint64(cReportSlack*float64(r.params.ClockRate))
		ubound := expectedRTPTimestampExt + uint64(cReportSlack*float64(r.params.ClockRate))
		isInRange := (srData.RTPTimestamp-uint32(lbound) < (1 << 31)) && (uint32(ubound)-srData.RTPTimestamp < (1 << 31))
		if isInRange {
			lbTSCycles := lbound & 0xFFFF_FFFF_0000_0000
			ubTSCycles := ubound & 0xFFFF_FFFF_0000_0000
			if lbTSCycles == ubTSCycles {
				tsCycles = lbTSCycles
			} else {
				if srData.RTPTimestamp < (1 << 31) {
					// rolled over
					tsCycles = ubTSCycles
				} else {
					tsCycles = lbTSCycles
				}
			}
		} else {
			// ideally this method should not be required, but there are clients
			// negotiating one clock rate, but actually send media at a different rate.
			tsCycles = r.srNewest.RTPTimestampExt & 0xFFFF_FFFF_0000_0000
			if (srData.RTPTimestamp-r.srNewest.RTPTimestamp) < (1<<31) && srData.RTPTimestamp < r.srNewest.RTPTimestamp {
				tsCycles += (1 << 32)
			}

			if tsCycles >= (1 << 32) {
				if (srData.RTPTimestamp-r.srNewest.RTPTimestamp) >= (1<<31) && srData.RTPTimestamp > r.srNewest.RTPTimestamp {
					tsCycles -= (1 << 32)
				}
			}
		}
	}

	srDataExt := *srData
	srDataExt.RTPTimestampExt = uint64(srDataExt.RTPTimestamp) + tsCycles
	return &srDataExt
}

func (r *RTPStatsReceiver) checkOutOfOrderSenderReport(srData *RTCPSenderReportData) bool {
	if r.srNewest != nil && srData.RTPTimestampExt < r.srNewest.RTPTimestampExt {
		// This can happen when a track is replaced with a null and then restored -
		// i. e. muting replacing with null and unmute restoring the original track.
		// Or it could be due bad report generation.
		// In any case, ignore out-of-order reports.
		r.outOfOrderSenderReportCount++
		if (r.outOfOrderSenderReportCount-1)%10 == 0 {
			r.logger.Infow(
				"received sender report, out-of-order, skipping",
				"current", srData,
				"count", r.outOfOrderSenderReportCount,
				"rtpStats", lockedRTPStatsReceiverLogEncoder{r},
			)
		}
		return true
	}

	return false
}

func (r *RTPStatsReceiver) checkRTPClockSkewForSenderReport(srData *RTCPSenderReportData) {
	if r.srNewest == nil {
		return
	}

	timeSinceLast := srData.NTPTimestamp.Time().Sub(r.srNewest.NTPTimestamp.Time()).Seconds()
	rtpDiffSinceLast := srData.RTPTimestampExt - r.srNewest.RTPTimestampExt
	calculatedClockRateFromLast := float64(rtpDiffSinceLast) / timeSinceLast

	timeSinceFirst := srData.NTPTimestamp.Time().Sub(r.srFirst.NTPTimestamp.Time()).Seconds()
	rtpDiffSinceFirst := srData.RTPTimestampExt - r.srFirst.RTPTimestampExt
	calculatedClockRateFromFirst := float64(rtpDiffSinceFirst) / timeSinceFirst

	if (timeSinceLast > 0.2 && math.Abs(float64(r.params.ClockRate)-calculatedClockRateFromLast) > 0.2*float64(r.params.ClockRate)) ||
		(timeSinceFirst > 0.2 && math.Abs(float64(r.params.ClockRate)-calculatedClockRateFromFirst) > 0.2*float64(r.params.ClockRate)) {
		r.clockSkewCount++
		if (r.clockSkewCount-1)%100 == 0 {
			r.logger.Infow(
				"received sender report, clock skew",
				"current", srData,
				"timeSinceFirst", timeSinceFirst,
				"rtpDiffSinceFirst", rtpDiffSinceFirst,
				"calculatedFirst", calculatedClockRateFromFirst,
				"timeSinceLast", timeSinceLast,
				"rtpDiffSinceLast", rtpDiffSinceLast,
				"calculatedLast", calculatedClockRateFromLast,
				"count", r.clockSkewCount,
				"rtpStats", lockedRTPStatsReceiverLogEncoder{r},
			)
		}
	}
}

func (r *RTPStatsReceiver) checkRTPClockSkewAgainstMediaPathForSenderReport(srData *RTCPSenderReportData) {
	if r.highestTime == 0 {
		return
	}

	timeSinceSR := time.Since(srData.AtAdjusted)
	extNowTSSR := srData.RTPTimestampExt + uint64(timeSinceSR.Nanoseconds()*int64(r.params.ClockRate)/1e9)

	timeSinceHighest := time.Since(time.Unix(0, r.highestTime))
	extNowTSHighest := r.timestamp.GetExtendedHighest() + uint64(timeSinceHighest.Nanoseconds()*int64(r.params.ClockRate)/1e9)
	diffHighest := extNowTSSR - extNowTSHighest

	timeSinceFirst := time.Since(time.Unix(0, r.firstTime))
	extNowTSFirst := r.timestamp.GetExtendedStart() + uint64(timeSinceFirst.Nanoseconds()*int64(r.params.ClockRate)/1e9)
	diffFirst := extNowTSSR - extNowTSFirst

	// is it more than 5 seconds off?
	if uint32(math.Abs(float64(int64(diffHighest)))) > 5*r.params.ClockRate || uint32(math.Abs(float64(int64(diffFirst)))) > 5*r.params.ClockRate {
		r.clockSkewMediaPathCount++
		if (r.clockSkewMediaPathCount-1)%100 == 0 {
			r.logger.Infow(
				"received sender report, clock skew against media path",
				"current", srData,
				"timeSinceSR", timeSinceSR,
				"extNowTSSR", extNowTSSR,
				"timeSinceHighest", timeSinceHighest,
				"extNowTSHighest", extNowTSHighest,
				"diffHighest", int64(diffHighest),
				"timeSinceFirst", timeSinceFirst,
				"extNowTSFirst", extNowTSFirst,
				"diffFirst", int64(diffFirst),
				"count", r.clockSkewMediaPathCount,
				"rtpStats", lockedRTPStatsReceiverLogEncoder{r},
			)
		}
	}
}

func (r *RTPStatsReceiver) updatePropagationDelayAndRecordSenderReport(srData *RTCPSenderReportData) {
	senderClockTime := srData.NTPTimestamp.Time()
	estimatedPropagationDelay, stepChange := r.propagationDelayEstimator.Update(senderClockTime, srData.At)
	if stepChange {
		r.logger.Debugw(
			"propagation delay step change",
			"currentSenderReport", srData,
			"rtpStats", lockedRTPStatsReceiverLogEncoder{r},
		)
	}

	if r.srFirst == nil {
		r.srFirst = srData
	}
	// adjust receive time to estimated propagation delay
	srData.AtAdjusted = senderClockTime.Add(estimatedPropagationDelay)
	r.srNewest = srData
}

func (r *RTPStatsReceiver) SetRtcpSenderReportData(srData *RTCPSenderReportData) bool {
	r.lock.Lock()
	defer r.lock.Unlock()

	if srData == nil || !r.initialized {
		return false
	}

	// prevent against extreme case of anachronous sender reports
	if r.srNewest != nil && r.srNewest.NTPTimestamp > srData.NTPTimestamp {
		r.logger.Infow(
			"received sender report, anachronous, dropping",
			"current", srData,
			"rtpStats", lockedRTPStatsReceiverLogEncoder{r},
		)
		return false
	}

	srDataExt := r.getExtendedSenderReport(srData)

	if r.checkOutOfOrderSenderReport(srDataExt) {
		return false
	}

	r.checkRTPClockSkewForSenderReport(srDataExt)
	r.updatePropagationDelayAndRecordSenderReport(srDataExt)
	r.checkRTPClockSkewAgainstMediaPathForSenderReport(srDataExt)

	if err, loggingFields := r.maybeAdjustFirstPacketTime(r.srNewest, 0, r.timestamp.GetExtendedStart()); err != nil {
		r.logger.Infow(err.Error(), append(loggingFields, "rtpStats", lockedRTPStatsReceiverLogEncoder{r})...)
	}
	return true
}

func (r *RTPStatsReceiver) GetRtcpSenderReportData() *RTCPSenderReportData {
	r.lock.RLock()
	defer r.lock.RUnlock()

	if r.srNewest == nil {
		return nil
	}

	srNewestCopy := *r.srNewest
	return &srNewestCopy
}

func (r *RTPStatsReceiver) LastSenderReportTime() time.Time {
	r.lock.RLock()
	defer r.lock.RUnlock()

	if r.srNewest != nil {
		return r.srNewest.At
	}

	return time.Time{}
}

func (r *RTPStatsReceiver) GetRtcpReceptionReport(ssrc uint32, proxyFracLost uint8, snapshotID uint32) *rtcp.ReceptionReport {
	r.lock.Lock()
	defer r.lock.Unlock()

	extHighestSN := r.sequenceNumber.GetExtendedHighest()
	then, now := r.getAndResetSnapshot(snapshotID, r.sequenceNumber.GetExtendedStart(), extHighestSN)
	if now == nil || then == nil {
		return nil
	}

	packetsExpected := now.extStartSN - then.extStartSN
	if packetsExpected > cNumSequenceNumbers {
		r.logger.Warnw(
			"too many packets expected in receiver report",
			fmt.Errorf("start: %d, end: %d, expected: %d", then.extStartSN, now.extStartSN, packetsExpected),
			"rtpStats", lockedRTPStatsReceiverLogEncoder{r},
		)
		return nil
	}
	if packetsExpected == 0 {
		return nil
	}

	packetsLost := uint32(now.packetsLost - then.packetsLost)
	if int32(packetsLost) < 0 {
		packetsLost = 0
	}
	lossRate := float32(packetsLost) / float32(packetsExpected)
	fracLost := uint8(lossRate * 256.0)
	if proxyFracLost > fracLost {
		fracLost = proxyFracLost
	}

	totalLost := r.packetsLost
	if totalLost > 0xffffff { // 24-bits max
		totalLost = 0xffffff
	}

	lastSR := uint32(0)
	dlsr := uint32(0)
	if r.srNewest != nil {
		lastSR = uint32(r.srNewest.NTPTimestamp >> 16)
		if !r.srNewest.At.IsZero() {
			delayUS := time.Since(r.srNewest.At).Microseconds()
			dlsr = uint32(delayUS * 65536 / 1e6)
		}
	}

	return &rtcp.ReceptionReport{
		SSRC:               ssrc,
		FractionLost:       fracLost,
		TotalLost:          uint32(totalLost),
		LastSequenceNumber: uint32(now.extStartSN),
		Jitter:             uint32(r.jitter),
		LastSenderReport:   lastSR,
		Delay:              dlsr,
	}
}

func (r *RTPStatsReceiver) DeltaInfo(snapshotID uint32) *RTPDeltaInfo {
	r.lock.Lock()
	defer r.lock.Unlock()

	deltaInfo, err, loggingFields := r.deltaInfo(
		snapshotID,
		r.sequenceNumber.GetExtendedStart(),
		r.sequenceNumber.GetExtendedHighest(),
	)
	if err != nil {
		r.logger.Infow(err.Error(), append(loggingFields, "rtpStats", lockedRTPStatsReceiverLogEncoder{r})...)
	}

	return deltaInfo
}

func (r *RTPStatsReceiver) MarshalLogObject(e zapcore.ObjectEncoder) error {
	if r == nil {
		return nil
	}

	r.lock.RLock()
	defer r.lock.RUnlock()

	return lockedRTPStatsReceiverLogEncoder{r}.MarshalLogObject(e)
}

func (r *RTPStatsReceiver) String() string {
	r.lock.RLock()
	defer r.lock.RUnlock()

	return r.toString(
		r.sequenceNumber.GetExtendedStart(), r.sequenceNumber.GetExtendedHighest(), r.timestamp.GetExtendedStart(), r.timestamp.GetExtendedHighest(),
		r.packetsLost,
		r.jitter, r.maxJitter,
	)
}

func (r *RTPStatsReceiver) ToProto() *livekit.RTPStats {
	r.lock.RLock()
	defer r.lock.RUnlock()

	return r.toProto(
		r.sequenceNumber.GetExtendedStart(), r.sequenceNumber.GetExtendedHighest(), r.timestamp.GetExtendedStart(), r.timestamp.GetExtendedHighest(),
		r.packetsLost,
		r.jitter, r.maxJitter,
	)
}

func (r *RTPStatsReceiver) isInRange(esn uint64, ehsn uint64) bool {
	diff := int64(ehsn - esn)
	return diff >= 0 && diff < cHistorySize
}

func (r *RTPStatsReceiver) HighestTimestamp() uint32 {
	r.lock.RLock()
	defer r.lock.RUnlock()

	return r.timestamp.GetHighest()
}

// ----------------------------------

type lockedRTPStatsReceiverLogEncoder struct {
	*RTPStatsReceiver
}

func (r lockedRTPStatsReceiverLogEncoder) MarshalLogObject(e zapcore.ObjectEncoder) error {
	if r.RTPStatsReceiver == nil {
		return nil
	}

	e.AddObject("base", r.rtpStatsBase)

	e.AddUint64("extStartSN", r.sequenceNumber.GetExtendedStart())
	e.AddUint64("extHighestSN", r.sequenceNumber.GetExtendedHighest())
	e.AddUint64("extStartTS", r.timestamp.GetExtendedStart())
	e.AddUint64("extHighestTS", r.timestamp.GetExtendedHighest())

	e.AddObject("propagationDelayEstimator", r.propagationDelayEstimator)
	return nil
}

// ----------------------------------
