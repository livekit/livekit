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

package rtpstats

import (
	"fmt"
	"math"
	"time"

	"github.com/pion/rtcp"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/mediatransportutil"
	"github.com/livekit/mediatransportutil/pkg/latency"
	"github.com/livekit/mediatransportutil/pkg/utils"
	"github.com/livekit/protocol/livekit"
	protoutils "github.com/livekit/protocol/utils"
	"github.com/livekit/protocol/utils/mono"
)

const (
	cHistorySize = 8192

	// number of seconds the current report RTP timestamp can be off from expected RTP timestamp
	cReportSlack = float64(60.0)

	cTSJumpTooHighFactor = float64(1.5)

	restartThreshold = 5

	// a run of rejected packets must last this long, with no accepted packet, to be a restart.
	// Restarts are rare, the wait covers enough packets to tell a restart from a burst of bad packets.
	restartMinDuration = int64(time.Second)

	// sequence number gap allowed inside a run of rejected packets, covers loss and padding
	restartMaxSNGap = 16
)

// ---------------------------------------------------------------------

type RTPFlowUnhandledReason int

const (
	RTPFlowUnhandledReasonNone RTPFlowUnhandledReason = iota
	RTPFlowUnhandledReasonEnded
	RTPFlowUnhandledReasonUnconfigured
	RTPFlowUnhandledReasonPaddingOnly
	RTPFlowUnhandledReasonPreStartTimestamp
	RTPFlowUnhandledReasonOldTimestamp
	RTPFlowUnhandledReasonPreStartSequenceNumber
	RTPFlowUnhandledReasonOldSequenceNumber
	RTPFlowUnhandledReasonRestart
)

func (r RTPFlowUnhandledReason) String() string {
	switch r {
	case RTPFlowUnhandledReasonNone:
		return "NONE"
	case RTPFlowUnhandledReasonEnded:
		return "ENDED"
	case RTPFlowUnhandledReasonUnconfigured:
		return "UNCONFIGURED"
	case RTPFlowUnhandledReasonPaddingOnly:
		return "PADDING_ONLY"
	case RTPFlowUnhandledReasonPreStartTimestamp:
		return "PRE_START_TIMESTAMP"
	case RTPFlowUnhandledReasonOldTimestamp:
		return "OLD_TIMESTAMP"
	case RTPFlowUnhandledReasonPreStartSequenceNumber:
		return "PRE_START_SEQUENCE_NUMBER"
	case RTPFlowUnhandledReasonOldSequenceNumber:
		return "OLD_SEQUENCE_NUMBER"
	case RTPFlowUnhandledReasonRestart:
		return "RESTART"
	default:
		return fmt.Sprintf("UNKNOWN: %d", int(r))
	}
}

type RTPFlowState struct {
	UnhandledReason RTPFlowUnhandledReason

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

	e.AddString("UnhandledReason", r.UnhandledReason.String())
	e.AddUint64("LossStartInclusive", r.LossStartInclusive)
	e.AddUint64("LossEndExclusive", r.LossEndExclusive)
	e.AddBool("IsDuplicate", r.IsDuplicate)
	e.AddBool("IsOutOfOrder", r.IsOutOfOrder)
	e.AddUint64("ExtSequenceNumber", r.ExtSequenceNumber)
	e.AddUint64("ExtTimestamp", r.ExtTimestamp)
	return nil
}

// ---------------------------------------------------------------------

type packet struct {
	sequenceNumber uint16
	timestamp      uint32
}

func (p packet) MarshalLogObject(e zapcore.ObjectEncoder) error {
	e.AddUint16("sequenceNumber", p.sequenceNumber)
	e.AddUint32("timestamp", p.timestamp)
	return nil
}

// ---------------------------------------------------------------------

// restartRun is a run of rejected packets that move forward like a stream.
type restartRun struct {
	startTime  int64
	first      packet
	last       packet
	count      int
	isDeclared bool
}

func (rr *restartRun) MarshalLogObject(e zapcore.ObjectEncoder) error {
	if rr == nil || rr.count == 0 {
		return nil
	}

	e.AddTime("startTime", time.Unix(0, rr.startTime))
	e.AddObject("first", rr.first)
	e.AddObject("last", rr.last)
	e.AddInt("count", rr.count)
	e.AddBool("isDeclared", rr.isDeclared)
	return nil
}

// ---------------------------------------------------------------------

type receiverUpdateLoggingFields struct {
	packetTime       int64
	sequenceNumber   uint16
	timestamp        uint32
	marker           bool
	hdrSize          int
	payloadSize      int
	paddingSize      int
	resSN            utils.WrapAroundUpdateResult[uint64]
	gapSN            int64
	resTS            utils.WrapAroundUpdateResult[uint64]
	gapTS            int64
	snRolloverCount  int
	expectedTSJump   int64
	tsRolloverCount  int
	timeSinceHighest int64
	rtpStats         *RTPStatsReceiver
}

func (rulf *receiverUpdateLoggingFields) MarshalLogObject(e zapcore.ObjectEncoder) error {
	if rulf != nil {
		e.AddObject("resSN", &rulf.resSN)
		e.AddInt64("gapSN", rulf.gapSN)
		e.AddObject("resTS", &rulf.resTS)
		e.AddInt64("gapTS", rulf.gapTS)
		e.AddInt("snRolloverCount", rulf.snRolloverCount)
		e.AddInt64("expectedTSJump", rulf.expectedTSJump)
		e.AddInt("tsRolloverCount", rulf.tsRolloverCount)
		e.AddTime("packetTime", time.Unix(0, rulf.packetTime))
		e.AddDuration("timeSinceHighest", time.Duration(rulf.timeSinceHighest))
		e.AddUint16("sequenceNumber", rulf.sequenceNumber)
		e.AddUint32("timestamp", rulf.timestamp)
		e.AddBool("marker", rulf.marker)
		e.AddInt("hdrSize", rulf.hdrSize)
		e.AddInt("payloadSize", rulf.payloadSize)
		e.AddInt("paddingSize", rulf.paddingSize)
		e.AddObject("rtpStats", lockedRTPStatsReceiverLogEncoder{rulf.rtpStats})
	}
	return nil
}

// ---------------------------------------------------------------------

type RTPStatsReceiver struct {
	*rtpStatsBase

	sequenceNumber *utils.WrapAround[uint16, uint64]

	tsRolloverThreshold int64
	timestamp           *utils.WrapAround[uint32, uint64]

	history *protoutils.Bitmap[uint64]

	propagationDelayEstimator *latency.OWDEstimator

	clockSkewCount              int
	clockSkewMediaPathCount     int
	outOfOrderSenderReportCount int

	packetsDroppedPreStartTimestamp      int
	packetsDroppedOldTimestamp           int
	packetsDroppedPreStartSequenceNumber int
	packetsDroppedOldSequenceNumber      int

	restartRun restartRun
}

func NewRTPStatsReceiver(params RTPStatsParams) *RTPStatsReceiver {
	return &RTPStatsReceiver{
		rtpStatsBase:              newRTPStatsBase(params),
		sequenceNumber:            utils.NewWrapAround[uint16, uint64](utils.WrapAroundParams{IsRestartAllowed: false}),
		timestamp:                 utils.NewWrapAround[uint32, uint64](utils.WrapAroundParams{IsRestartAllowed: false}),
		history:                   protoutils.NewBitmap[uint64](cHistorySize),
		propagationDelayEstimator: latency.NewOWDEstimator(latency.OWDEstimatorParamsDefault),
	}
}

func (r *RTPStatsReceiver) SetClockRate(clockRate uint32) {
	r.lock.Lock()
	defer r.lock.Unlock()

	r.tsRolloverThreshold = (1 << 31) * 1e9 / int64(clockRate)
	r.rtpStatsBase.setClockRateLocked(clockRate)
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

	excess := (diffNano - r.tsRolloverThreshold*2) * int64(r.clockRate) / 1e9
	roc := max(excess/(1<<32), 0)
	if r.timestamp.GetHighest() > ts {
		roc++
	}
	return int(roc)
}

func (r *RTPStatsReceiver) undoUpdatesLocked(resSN utils.WrapAroundUpdateResult[uint64], resTS utils.WrapAroundUpdateResult[uint64]) {
	r.sequenceNumber.UndoUpdate(resSN)
	r.timestamp.UndoUpdate(resTS)
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

	if r.endTime != 0 {
		flowState.UnhandledReason = RTPFlowUnhandledReasonEnded
		return
	}
	if r.clockRate == 0 {
		flowState.UnhandledReason = RTPFlowUnhandledReasonUnconfigured
		return
	}

	var resSN utils.WrapAroundUpdateResult[uint64]
	var gapSN int64
	var resTS utils.WrapAroundUpdateResult[uint64]
	var gapTS int64
	var expectedTSJump int64
	var timeSinceHighest int64
	var tsRolloverCount int
	var snRolloverCount int

	if !r.initialized {
		if payloadSize == 0 {
			// do not start on a padding only packet
			flowState.UnhandledReason = RTPFlowUnhandledReasonPaddingOnly
			return
		}

		r.initialized = true

		r.startTime = mono.UnixNano()

		r.firstTime = packetTime
		r.highestTime = packetTime

		resSN = r.sequenceNumber.Update(sequenceNumber)
		resTS = r.timestamp.Update(timestamp)

		// initialize snapshots if any
		for i := uint32(0); i < r.nextSnapshotID-cFirstSnapshotID; i++ {
			r.snapshots[i] = initSnapshot(r.startTime, r.sequenceNumber.GetExtendedStart())
		}

		r.logger.Debugw(
			"rtp receiver stream start",
			"rtpStats", lockedRTPStatsReceiverLogEncoder{r},
		)
	} else {
		loggingFields := func() *receiverUpdateLoggingFields {
			return &receiverUpdateLoggingFields{
				packetTime:       packetTime,
				sequenceNumber:   sequenceNumber,
				timestamp:        timestamp,
				marker:           marker,
				hdrSize:          hdrSize,
				payloadSize:      payloadSize,
				paddingSize:      paddingSize,
				resSN:            resSN,
				gapSN:            gapSN,
				resTS:            resTS,
				gapTS:            gapTS,
				snRolloverCount:  snRolloverCount,
				expectedTSJump:   expectedTSJump,
				tsRolloverCount:  tsRolloverCount,
				timeSinceHighest: timeSinceHighest,
				rtpStats:         r,
			}
		}

		resSN = r.sequenceNumber.Update(sequenceNumber)
		gapSN = int64(resSN.ExtendedVal - resSN.PreExtendedHighest)

		timeSinceHighest = packetTime - r.highestTime
		expectedTSJump = int64(r.rtpConverter.ToRTPExt(time.Duration(timeSinceHighest)))
		tsRolloverCount = r.getTSRolloverCount(timeSinceHighest, timestamp)
		resTS = r.timestamp.Rollover(timestamp, tsRolloverCount)
		gapTS = int64(resTS.ExtendedVal - resTS.PreExtendedHighest)
		if resTS.IsUnhandled {
			flowState.UnhandledReason = r.dropPacketLocked(
				loggingFields(),
				&r.packetsDroppedPreStartTimestamp,
				"dropping packet, pre-start timestamp",
				RTPFlowUnhandledReasonPreStartTimestamp,
			)
			return
		}

		if tsRolloverCount >= 0 && payloadSize > 0 {
			r.logger.Warnw("potential time stamp roll over", nil, zap.Inline(loggingFields()))
		}

		if !resSN.IsUnhandled {
			// it is possible to receive old packets in two different scenarios
			// as it is not possible to detect how far to roll back, ignore old packets
			//
			// Case 1:
			//  Very old time stamp, happens under the following conditions
			//  - resume after long mute, this causes big time stamp jump ahead for the packets
			//    after unmute
			//  - an out of order packet from before the mute arrives (unsure what causes this
			//    very old packet to be transmitted from remote), causing time stamp to jump back
			//    to before mute, but it appears like it has rolled over.
			//  Use a threshold against expected to ignore these.
			if gapSN < 0 && gapTS > 0 && gapTS > int64(float64(expectedTSJump)*cTSJumpTooHighFactor) {
				flowState.UnhandledReason = r.dropPacketLocked(
					loggingFields(),
					&r.packetsDroppedOldTimestamp,
					"dropping packet, old timestamp",
					RTPFlowUnhandledReasonOldTimestamp,
				)
				return
			}

			// Case 2:
			//  Sequence number moves forward, but the time stamp goes back.
			//  Either a very old packet or a publisher that stepped its time stamp back.
			if gapTS < 0 && gapSN > 0 {
				flowState.UnhandledReason = r.dropPacketLocked(
					loggingFields(),
					&r.packetsDroppedOldSequenceNumber,
					"dropping packet, old sequence number",
					RTPFlowUnhandledReasonOldSequenceNumber,
				)
				return
			}
		}

		// it is possible that sequence number has rolled over too
		if (gapSN < 0 || gapSN > (1<<15)) && gapTS > 0 && payloadSize > 0 {
			// not possible to know how many cycles of sequence number roll over could have happened,
			// ensure that it at least does not go backwards
			snRolloverCount = 0
			if sequenceNumber < r.sequenceNumber.GetHighest() {
				snRolloverCount = 1
			}
			resSN = r.sequenceNumber.Rollover(sequenceNumber, snRolloverCount)
			if !resSN.IsUnhandled {
				r.logger.Warnw("forcing sequence number rollover", nil, zap.Inline(loggingFields()))
			}
		}

		if resSN.IsUnhandled {
			flowState.UnhandledReason = r.dropPacketLocked(
				loggingFields(),
				&r.packetsDroppedPreStartSequenceNumber,
				"dropping packet, pre-start sequence number",
				RTPFlowUnhandledReasonPreStartSequenceNumber,
			)
			return
		}
	}
	gapSN = int64(resSN.ExtendedVal - resSN.PreExtendedHighest)

	pktSize := uint64(hdrSize + payloadSize + paddingSize)
	if gapSN <= 0 { // duplicate OR out-of-order
		if gapSN != 0 {
			r.packetsOutOfOrder++
		}

		if r.isInRange(resSN.ExtendedVal, resSN.PreExtendedHighest) {
			if r.history.GetAndSet(resSN.ExtendedVal) {
				r.bytesDuplicate += pktSize
				r.headerBytesDuplicate += uint64(hdrSize)
				r.packetsDuplicate++
				flowState.IsDuplicate = true
			} else {
				r.packetsLost--
			}
		}

		flowState.IsOutOfOrder = true
	} else { // in-order
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

		flowState.LossStartInclusive = resSN.PreExtendedHighest + 1
		flowState.LossEndExclusive = resSN.ExtendedVal
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
	// skipped sequence numbers are reported with no payload, they do not show that the stream is alive
	if payloadSize > 0 {
		r.resetRestart(packetTime)
	}
	return
}

func (r *RTPStatsReceiver) getExtendedSenderReport(srData *livekit.RTCPSenderReportState) *livekit.RTCPSenderReportState {
	tsCycles := uint64(0)
	if r.srNewest != nil {
		// use time since last sender report to ensure long gaps where the time stamp might
		// jump more than half the range
		srTime := mediatransportutil.NtpTime(srData.NtpTimestamp).Time()
		srNewestTime := mediatransportutil.NtpTime(r.srNewest.NtpTimestamp).Time()
		timeSinceLastReport := srTime.Sub(srNewestTime)
		expectedRTPTimestampExt := r.srNewest.RtpTimestampExt + r.rtpConverter.ToRTPExt(timeSinceLastReport)
		lbound := expectedRTPTimestampExt - uint64(cReportSlack*float64(r.clockRate))
		ubound := expectedRTPTimestampExt + uint64(cReportSlack*float64(r.clockRate))
		isInRange := (srData.RtpTimestamp-uint32(lbound) < (1 << 31)) && (uint32(ubound)-srData.RtpTimestamp < (1 << 31))
		if isInRange {
			lbTSCycles := lbound & 0xFFFF_FFFF_0000_0000
			ubTSCycles := ubound & 0xFFFF_FFFF_0000_0000
			if lbTSCycles == ubTSCycles {
				tsCycles = lbTSCycles
			} else {
				if srData.RtpTimestamp < (1 << 31) {
					// rolled over
					tsCycles = ubTSCycles
				} else {
					tsCycles = lbTSCycles
				}
			}
		} else {
			// ideally this method should not be required, but there are clients
			// negotiating one clock rate, but actually send media at a different rate.
			tsCycles = r.srNewest.RtpTimestampExt & 0xFFFF_FFFF_0000_0000
			if (srData.RtpTimestamp-r.srNewest.RtpTimestamp) < (1<<31) && srData.RtpTimestamp < r.srNewest.RtpTimestamp {
				tsCycles += (1 << 32)
			}

			if tsCycles >= (1 << 32) {
				if (srData.RtpTimestamp-r.srNewest.RtpTimestamp) >= (1<<31) && srData.RtpTimestamp > r.srNewest.RtpTimestamp {
					tsCycles -= (1 << 32)
				}
			}
		}
	}

	srDataExt := proto.CloneOf(srData)
	srDataExt.RtpTimestampExt = uint64(srDataExt.RtpTimestamp) + tsCycles
	return srDataExt
}

func (r *RTPStatsReceiver) checkOutOfOrderSenderReport(srData *livekit.RTCPSenderReportState) bool {
	if r.srNewest != nil && srData.RtpTimestampExt < r.srNewest.RtpTimestampExt {
		// This can happen when a track is replaced with a null and then restored -
		// i. e. muting replacing with null and unmute restoring the original track.
		// Or it could be due bad report generation.
		// In any case, ignore out-of-order reports.
		r.outOfOrderSenderReportCount++
		if shouldLog(r.outOfOrderSenderReportCount) {
			r.logger.Infow(
				"received sender report, out-of-order, skipping",
				"current", WrappedRTCPSenderReportStateLogger{srData},
				"count", r.outOfOrderSenderReportCount,
				"rtpStats", lockedRTPStatsReceiverLogEncoder{r},
			)
		}
		return true
	}

	return false
}

func (r *RTPStatsReceiver) checkRTPClockSkewForSenderReport(srData *livekit.RTCPSenderReportState) {
	if r.srNewest == nil {
		return
	}

	srTime := mediatransportutil.NtpTime(srData.NtpTimestamp).Time()
	srNewestTime := mediatransportutil.NtpTime(r.srNewest.NtpTimestamp).Time()
	srFirstTime := mediatransportutil.NtpTime(r.srFirst.NtpTimestamp).Time()

	timeSinceLast := srTime.Sub(srNewestTime).Seconds()
	rtpDiffSinceLast := srData.RtpTimestampExt - r.srNewest.RtpTimestampExt
	calculatedClockRateFromLast := float64(rtpDiffSinceLast) / timeSinceLast

	timeSinceFirst := srTime.Sub(srFirstTime).Seconds()
	rtpDiffSinceFirst := srData.RtpTimestampExt - r.srFirst.RtpTimestampExt
	calculatedClockRateFromFirst := float64(rtpDiffSinceFirst) / timeSinceFirst

	if (timeSinceLast > 0.2 && math.Abs(float64(r.clockRate)-calculatedClockRateFromLast) > 0.2*float64(r.clockRate)) ||
		(timeSinceFirst > 0.2 && math.Abs(float64(r.clockRate)-calculatedClockRateFromFirst) > 0.2*float64(r.clockRate)) {
		r.clockSkewCount++
		if shouldLog(r.clockSkewCount) {
			r.logger.Infow(
				"received sender report, clock skew",
				"current", WrappedRTCPSenderReportStateLogger{srData},
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

func (r *RTPStatsReceiver) checkRTPClockSkewAgainstMediaPathForSenderReport(srData *livekit.RTCPSenderReportState) {
	if r.highestTime == 0 {
		return
	}

	nowNano := mono.UnixNano()
	timeSinceSR := time.Duration(nowNano - srData.AtAdjusted)
	extNowTSSR := srData.RtpTimestampExt + r.rtpConverter.ToRTPExt(timeSinceSR)

	timeSinceHighest := time.Duration(nowNano - r.highestTime)
	extNowTSHighest := r.timestamp.GetExtendedHighest() + r.rtpConverter.ToRTPExt(timeSinceHighest)
	diffHighest := extNowTSSR - extNowTSHighest

	timeSinceFirst := time.Duration(nowNano - r.firstTime)
	extNowTSFirst := r.timestamp.GetExtendedStart() + r.rtpConverter.ToRTPExt(timeSinceFirst)
	diffFirst := extNowTSSR - extNowTSFirst

	// is it more than 5 seconds off?
	if uint32(math.Abs(float64(int64(diffHighest)))) > 5*r.clockRate || uint32(math.Abs(float64(int64(diffFirst)))) > 5*r.clockRate {
		r.clockSkewMediaPathCount++
		if shouldLog(r.clockSkewMediaPathCount) {
			r.logger.Infow(
				"received sender report, clock skew against media path",
				"current", WrappedRTCPSenderReportStateLogger{srData},
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

func (r *RTPStatsReceiver) updatePropagationDelayAndRecordSenderReport(srData *livekit.RTCPSenderReportState) {
	srTime := mediatransportutil.NtpTime(srData.NtpTimestamp).Time()
	senderClockTime := srTime.UnixNano()
	estimatedPropagationDelay, stepChange := r.propagationDelayEstimator.Update(senderClockTime, srData.At)
	if stepChange {
		r.logger.Debugw(
			"propagation delay step change",
			"currentSenderReport", WrappedRTCPSenderReportStateLogger{srData},
			"rtpStats", lockedRTPStatsReceiverLogEncoder{r},
		)
	}

	if r.srFirst == nil {
		r.srFirst = srData
	}
	// adjust receive time to estimated propagation delay
	srData.AtAdjusted = senderClockTime + estimatedPropagationDelay
	r.srNewest = srData
}

func (r *RTPStatsReceiver) SetRtcpSenderReportData(srData *livekit.RTCPSenderReportState) bool {
	r.lock.Lock()
	defer r.lock.Unlock()

	if srData == nil || !r.initialized || r.clockRate == 0 {
		return false
	}

	// prevent against extreme case of anachronous sender reports
	if r.srNewest != nil && r.srNewest.NtpTimestamp > srData.NtpTimestamp {
		r.logger.Infow(
			"received sender report, anachronous, dropping",
			"current", WrappedRTCPSenderReportStateLogger{srData},
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

	adjustment, loggingFields, err := r.maybeAdjustFirstPacketTime(r.srNewest, 0, r.timestamp.GetExtendedStart())
	if err != nil {
		r.logger.Infow(err.Error(), append(loggingFields, "rtpStats", lockedRTPStatsReceiverLogEncoder{r})...)
	}
	r.propagationDelayEstimator.InitialAdjustment(adjustment)
	return true
}

func (r *RTPStatsReceiver) GetRtcpSenderReportData() *livekit.RTCPSenderReportState {
	r.lock.RLock()
	defer r.lock.RUnlock()

	return proto.CloneOf(r.srNewest)
}

func (r *RTPStatsReceiver) LastSenderReportTime() time.Time {
	r.lock.RLock()
	defer r.lock.RUnlock()

	if r.srNewest != nil && r.srNewest.At != 0 {
		// srNewest.At is a mono timestamp, convert to wall time for comparison with time.Now()
		return time.Now().Add(-time.Duration(mono.UnixNano() - r.srNewest.At))
	}

	return time.Time{}
}

func (r *RTPStatsReceiver) GetRtcpReceptionReport(ssrc uint32, proxyFracLost uint8, snapshotID uint32) *rtcp.ReceptionReport {
	r.lock.Lock()
	defer r.lock.Unlock()

	extHighestSN := r.sequenceNumber.GetExtendedHighest()
	then, now, ok := r.getAndResetSnapshot(snapshotID, r.sequenceNumber.GetExtendedStart(), extHighestSN)
	if !ok {
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
	fracLost := max(proxyFracLost, uint8(lossRate*256.0))

	totalLost := min(r.packetsLost, 0xffffff) // 24-bits max

	lastSR := uint32(0)
	dlsr := uint32(0)
	if r.srNewest != nil {
		lastSR = uint32(r.srNewest.NtpTimestamp >> 16)
		if r.srNewest.At != 0 {
			delayUS := (mono.UnixNano() - r.srNewest.At) / 1e3
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

	deltaInfo, loggingFields, err := r.deltaInfo(
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

func (r *RTPStatsReceiver) ToProto() *livekit.RTPStats {
	if r == nil {
		return nil
	}

	r.lock.RLock()
	defer r.lock.RUnlock()

	extStartSN, extHighestSN := r.sequenceNumber.GetExtendedStart(), r.sequenceNumber.GetExtendedHighest()
	return r.toProto(
		getPacketsExpected(extStartSN, extHighestSN),
		r.getPacketsSeenMinusPadding(extStartSN, extHighestSN),
		r.packetsLost,
		r.timestamp.GetExtendedStart(),
		r.timestamp.GetExtendedHighest(),
		r.jitter,
		r.maxJitter,
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

// for testing only
func (r *RTPStatsReceiver) HighestSequenceNumber() uint16 {
	r.lock.RLock()
	defer r.lock.RUnlock()

	return r.sequenceNumber.GetHighest()
}

// for testing only
func (r *RTPStatsReceiver) ExtendedHighestSequenceNumber() uint64 {
	r.lock.RLock()
	defer r.lock.RUnlock()

	return r.sequenceNumber.GetExtendedHighest()
}

// dropPacketLocked rolls back a rejected packet and checks if it is part of a restart.
func (r *RTPStatsReceiver) dropPacketLocked(
	rulf *receiverUpdateLoggingFields,
	dropped *int,
	msg string,
	reason RTPFlowUnhandledReason,
) RTPFlowUnhandledReason {
	r.undoUpdatesLocked(rulf.resSN, rulf.resTS)

	*dropped++
	if shouldLog(*dropped) {
		r.logger.Warnw(msg, nil, zap.Inline(rulf))
	}

	if r.maybeRestart(rulf.packetTime, rulf.sequenceNumber, rulf.timestamp, rulf.payloadSize) {
		r.logger.Infow("potential restart", zap.Inline(rulf), "reason", reason)
		return RTPFlowUnhandledReasonRestart
	}
	return reason
}

// maybeRestart reports a likely restart, false positives are possible.
// A declared run that ends with an accepted packet logs "stream recovered without restart".
func (r *RTPStatsReceiver) maybeRestart(packetTime int64, sn uint16, ts uint32, payloadSize int) bool {
	rr := &r.restartRun
	if payloadSize == 0 {
		// padding moves the run forward in sequence number, it does not count towards a restart
		if rr.count != 0 && sn-rr.last.sequenceNumber-1 < restartMaxSNGap {
			rr.last.sequenceNumber = sn
		}
		return false
	}

	// a reordered or duplicate packet of the run does not change the run
	if rr.count != 0 && rr.last.sequenceNumber-sn < restartMaxSNGap {
		return false
	}

	if rr.isDeclared {
		rr.last = packet{sn, ts}
		rr.count++
		return false
	}

	// a run moves forward in sequence number and does not go back in timestamp
	if rr.count == 0 || sn-rr.last.sequenceNumber-1 >= restartMaxSNGap || (ts-rr.last.timestamp) > (1<<31) {
		*rr = restartRun{startTime: packetTime, first: packet{sn, ts}}
	}
	rr.last = packet{sn, ts}
	rr.count++

	if rr.count < restartThreshold || packetTime-rr.startTime < restartMinDuration {
		return false
	}

	rr.isDeclared = true
	return true
}

func (r *RTPStatsReceiver) resetRestart(packetTime int64) {
	if r.restartRun.count == 0 {
		return
	}

	if r.restartRun.isDeclared {
		r.logger.Infow(
			"stream recovered without restart",
			"duration", time.Duration(packetTime-r.restartRun.startTime),
			"rtpStats", lockedRTPStatsReceiverLogEncoder{r},
		)
	}
	r.restartRun = restartRun{}
}

// ----------------------------------

type lockedRTPStatsReceiverLogEncoder struct {
	*RTPStatsReceiver
}

func (r lockedRTPStatsReceiverLogEncoder) MarshalLogObject(e zapcore.ObjectEncoder) error {
	if r.RTPStatsReceiver == nil {
		return nil
	}

	extStartSN, extHighestSN := r.sequenceNumber.GetExtendedStart(), r.sequenceNumber.GetExtendedHighest()
	extStartTS, extHighestTS := r.timestamp.GetExtendedStart(), r.timestamp.GetExtendedHighest()
	if _, err := r.rtpStatsBase.marshalLogObject(
		e,
		getPacketsExpected(extStartSN, extHighestSN),
		r.getPacketsSeenMinusPadding(extStartSN, extHighestSN),
		extStartTS,
		extHighestTS,
	); err != nil {
		return err
	}

	e.AddUint64("extStartSN", extStartSN)
	e.AddUint64("extHighestSN", extHighestSN)
	e.AddUint64("extStartTS", extStartTS)
	e.AddUint64("extHighestTS", extHighestTS)

	e.AddObject("propagationDelayEstimator", r.propagationDelayEstimator)

	e.AddInt("clockSkewCount", r.clockSkewCount)
	e.AddInt("clockSkewMediaPathCount", r.clockSkewMediaPathCount)
	e.AddInt("outOfOrderSenderReportCount", r.outOfOrderSenderReportCount)

	e.AddInt("packetsDroppedPreStartTimestamp", r.packetsDroppedPreStartTimestamp)
	e.AddInt("packetsDroppedOldTimestamp", r.packetsDroppedOldTimestamp)
	e.AddInt("packetsDroppedPreStartSequenceNumber", r.packetsDroppedPreStartSequenceNumber)
	e.AddInt("packetsDroppedOldSequenceNumber", r.packetsDroppedOldSequenceNumber)

	e.AddObject("restartRun", &r.restartRun)
	return nil
}

// ----------------------------------
