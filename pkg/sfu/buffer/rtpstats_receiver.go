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
	"time"

	"github.com/pion/rtcp"

	"github.com/livekit/livekit-server/pkg/sfu/utils"
	"github.com/livekit/protocol/livekit"
	protoutils "github.com/livekit/protocol/utils"
)

const (
	cHistorySize = 2048
)

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

type RTPStatsReceiver struct {
	*rtpStatsBase

	resyncOnNextPacket             bool
	shouldDiscountPaddingOnlyDrops bool

	sequenceNumber *utils.WrapAround[uint16, uint64]

	timestamp *utils.WrapAround[uint32, uint64]

	history *protoutils.Bitmap[uint64]
}

func NewRTPStatsReceiver(params RTPStatsParams) *RTPStatsReceiver {
	return &RTPStatsReceiver{
		rtpStatsBase:   newRTPStatsBase(params),
		sequenceNumber: utils.NewWrapAround[uint16, uint64](),
		timestamp:      utils.NewWrapAround[uint32, uint64](),
		history:        protoutils.NewBitmap[uint64](cHistorySize),
	}
}

func (r *RTPStatsReceiver) NewSnapshotId() uint32 {
	r.lock.Lock()
	defer r.lock.Unlock()

	return r.newSnapshotID(r.sequenceNumber.GetExtendedStart())
}

func (r *RTPStatsReceiver) Update(
	packetTime time.Time,
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

	if r.resyncOnNextPacket {
		r.resyncOnNextPacket = false
		r.resync(packetTime, sequenceNumber, timestamp)
	}

	var resSN utils.WrapAroundUpdateResult[uint64]
	var resTS utils.WrapAroundUpdateResult[uint64]
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
		for i := uint32(cFirstSnapshotID); i < r.nextSnapshotID; i++ {
			r.snapshots[i] = r.initSnapshot(r.startTime, r.sequenceNumber.GetExtendedStart())
		}

		r.logger.Debugw(
			"rtp receiver stream start",
			"startTime", r.startTime.String(),
			"firstTime", r.firstTime.String(),
			"startSN", r.sequenceNumber.GetExtendedStart(),
			"startTS", r.timestamp.GetExtendedStart(),
		)
	} else {
		resSN = r.sequenceNumber.Update(sequenceNumber)
		resTS = r.timestamp.Update(timestamp)
	}

	pktSize := uint64(hdrSize + payloadSize + paddingSize)
	gapSN := int64(resSN.ExtendedVal - resSN.PreExtendedHighest)
	if gapSN <= 0 { // duplicate OR out-of-order
		if payloadSize == 0 {
			// do not start on a padding only packet
			if resTS.IsRestart {
				r.logger.Infow("rolling back timestamp restart", "tsBefore", r.timestamp.GetExtendedStart(), "tsAfter", resTS.PreExtendedStart)
				r.timestamp.RollbackRestart(resTS.PreExtendedStart)
			}
			if resSN.IsRestart {
				r.logger.Infow("rolling back sequence number restart", "snBefore", r.sequenceNumber.GetExtendedStart(), "snAfter", resSN.PreExtendedStart)
				r.sequenceNumber.RollbackRestart(resSN.PreExtendedStart)
				return
			}
		}

		if gapSN != 0 {
			r.packetsOutOfOrder++
		}

		if resSN.IsRestart {
			r.packetsLost += resSN.PreExtendedStart - resSN.ExtendedVal

			extStartSN := r.sequenceNumber.GetExtendedStart()
			for _, s := range r.snapshots {
				if s.extStartSN == resSN.PreExtendedStart {
					s.extStartSN = extStartSN
				}
			}

			r.logger.Infow(
				"adjusting start sequence number",
				"snBefore", resSN.PreExtendedStart,
				"snAfter", resSN.ExtendedVal,
			)
		}

		if resTS.IsRestart {
			r.logger.Infow(
				"adjusting start timestamp",
				"tsBefore", resTS.PreExtendedStart,
				"tsAfter", resTS.ExtendedVal,
			)
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
		flowState.ExtSequenceNumber = resSN.ExtendedVal
		flowState.ExtTimestamp = resTS.ExtendedVal
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

		if gapSN > 1 {
			flowState.HasLoss = true
			flowState.LossStartInclusive = resSN.PreExtendedHighest + 1
			flowState.LossEndExclusive = resSN.ExtendedVal
		}
		flowState.ExtSequenceNumber = resSN.ExtendedVal
		flowState.ExtTimestamp = resTS.ExtendedVal
	}

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

func (r *RTPStatsReceiver) ResyncOnNextPacket(shouldDiscountPaddingOnlyDrops bool) {
	r.lock.Lock()
	defer r.lock.Unlock()

	r.resyncOnNextPacket = true
	r.shouldDiscountPaddingOnlyDrops = shouldDiscountPaddingOnlyDrops
}

func (r *RTPStatsReceiver) resync(packetTime time.Time, sn uint16, ts uint32) {
	if !r.initialized {
		return
	}

	extHighestSN := r.sequenceNumber.GetExtendedHighest()
	var newestPacketCount uint64
	var paddingOnlyDrops uint64
	var extExpectedHighestSN uint64
	var expectedHighestSN uint16
	var snCycles uint64

	extHighestTS := r.timestamp.GetExtendedHighest()
	var newestTS uint64
	var extExpectedHighestTS uint64
	var expectedHighestTS uint32
	var tsCycles uint64
	if r.srNewest != nil {
		newestPacketCount = r.srNewest.PacketCountExt
		paddingOnlyDrops = r.srNewest.PaddingOnlyDrops
		if newestPacketCount != 0 {
			extExpectedHighestSN = r.sequenceNumber.GetExtendedStart() + newestPacketCount
			if r.shouldDiscountPaddingOnlyDrops {
				extExpectedHighestSN -= paddingOnlyDrops
			}
			expectedHighestSN = uint16(extExpectedHighestSN & 0xFFFF)
			snCycles = extExpectedHighestSN & 0xFFFF_FFFF_FFFF_0000
			if sn-expectedHighestSN < (1<<15) && sn < expectedHighestSN {
				snCycles += (1 << 16)
			}
			if snCycles != 0 && expectedHighestSN-sn < (1<<15) && expectedHighestSN < sn {
				snCycles -= (1 << 16)
			}
		}

		newestTS = r.srNewest.RTPTimestampExt
		extExpectedHighestTS = newestTS
		expectedHighestTS = uint32(extExpectedHighestTS & 0xFFFF_FFFF)
		tsCycles = extExpectedHighestTS & 0xFFFF_FFFF_0000_0000
		if ts-expectedHighestTS < (1<<31) && ts < expectedHighestTS {
			tsCycles += (1 << 32)
		}
		if tsCycles != 0 && expectedHighestTS-ts < (1<<31) && expectedHighestTS < ts {
			tsCycles -= (1 << 32)
		}
	}
	r.sequenceNumber.ResetHighest(snCycles + uint64(sn) - 1)
	r.timestamp.ResetHighest(tsCycles + uint64(ts))
	r.highestTime = packetTime
	r.logger.Debugw(
		"resync",
		"newestPacketCount", newestPacketCount,
		"paddingOnlyDrops", paddingOnlyDrops,
		"extExpectedHighestSN", extExpectedHighestSN,
		"expectedHighestSN", expectedHighestSN,
		"snCycles", snCycles,
		"rtpSN", sn,
		"beforeExtHighestSN", extHighestSN,
		"afterExtHighestSN", r.sequenceNumber.GetExtendedHighest(),
		"newestTS", newestTS,
		"extExpectedHighestTS", extExpectedHighestTS,
		"expectedHighestTS", expectedHighestTS,
		"tsCycles", tsCycles,
		"rtpTS", ts,
		"beforeExtHighestTS", extHighestTS,
		"afterExtHighestTS", r.timestamp.GetExtendedHighest(),
	)
}

func (r *RTPStatsReceiver) SetRtcpSenderReportData(srData *RTCPSenderReportData) {
	r.lock.Lock()
	defer r.lock.Unlock()

	if srData == nil || !r.initialized {
		return
	}

	// prevent against extreme case of anachronous sender reports
	if r.srNewest != nil && r.srNewest.NTPTimestamp > srData.NTPTimestamp {
		r.logger.Infow(
			"received anachronous sender report",
			"currentNTP", srData.NTPTimestamp.Time().String(),
			"currentRTP", srData.RTPTimestamp,
			"currentAt", srData.At.String(),
			"lastNTP", r.srNewest.NTPTimestamp.Time().String(),
			"lastRTP", r.srNewest.RTPTimestamp,
			"lastAt", r.srNewest.At.String(),
		)
		return
	}

	tsCycles := uint64(0)
	pcCycles := uint64(0)
	if r.srNewest != nil {
		tsCycles = r.srNewest.RTPTimestampExt & 0xFFFF_FFFF_0000_0000
		if (srData.RTPTimestamp-r.srNewest.RTPTimestamp) < (1<<31) && srData.RTPTimestamp < r.srNewest.RTPTimestamp {
			tsCycles += (1 << 32)
		}

		pcCycles = r.srNewest.PacketCountExt & 0xFFFF_FFFF_0000_0000
		if (srData.PacketCount-r.srNewest.PacketCount) < (1<<31) && srData.PacketCount < r.srNewest.PacketCount {
			pcCycles += (1 << 32)
		}
	}

	srDataCopy := *srData
	srDataCopy.RTPTimestampExt = uint64(srDataCopy.RTPTimestamp) + tsCycles
	srDataCopy.PacketCountExt = uint64(srDataCopy.PacketCount) + pcCycles

	r.maybeAdjustFirstPacketTime(srDataCopy.RTPTimestampExt, r.timestamp.GetExtendedStart())

	if r.srNewest != nil && srDataCopy.RTPTimestampExt < r.srNewest.RTPTimestampExt {
		// This can happen when a track is replaced with a null and then restored -
		// i. e. muting replacing with null and unmute restoring the original track.
		// Under such a condition reset the sender reports to start from this point.
		// Resetting will ensure sample rate calculations do not go haywire due to negative time.
		r.logger.Infow(
			"received sender report, out-of-order, resetting",
			"prevTSExt", r.srNewest.RTPTimestampExt,
			"prevRTP", r.srNewest.RTPTimestamp,
			"prevNTP", r.srNewest.NTPTimestamp.Time().String(),
			"prevAt", r.srNewest.At.String(),
			"currTSExt", srDataCopy.RTPTimestampExt,
			"currRTP", srDataCopy.RTPTimestamp,
			"currNTP", srDataCopy.NTPTimestamp.Time().String(),
			"currentAt", srDataCopy.At.String(),
		)
		r.srFirst = nil
	}

	r.srNewest = &srDataCopy
	if r.srFirst == nil {
		r.srFirst = &srDataCopy
	}
}

func (r *RTPStatsReceiver) GetRtcpSenderReportData() (srFirst *RTCPSenderReportData, srNewest *RTCPSenderReportData) {
	r.lock.RLock()
	defer r.lock.RUnlock()

	if r.srFirst != nil {
		srFirstCopy := *r.srFirst
		srFirst = &srFirstCopy
	}

	if r.srNewest != nil {
		srNewestCopy := *r.srNewest
		srNewest = &srNewestCopy
	}
	return
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

	lastSR := uint32(0)
	dlsr := uint32(0)
	if r.srNewest != nil {
		lastSR = uint32(r.srNewest.NTPTimestamp >> 16)
		if !r.srNewest.At.IsZero() {
			delayMS := uint32(time.Since(r.srNewest.At).Milliseconds())
			dlsr = (delayMS / 1e3) << 16
			dlsr |= (delayMS % 1e3) * 65536 / 1000
		}
	}

	return &rtcp.ReceptionReport{
		SSRC:               ssrc,
		FractionLost:       fracLost,
		TotalLost:          uint32(r.packetsLost),
		LastSequenceNumber: uint32(now.extStartSN),
		Jitter:             uint32(r.jitter),
		LastSenderReport:   lastSR,
		Delay:              dlsr,
	}
}

func (r *RTPStatsReceiver) DeltaInfo(snapshotID uint32) *RTPDeltaInfo {
	r.lock.Lock()
	defer r.lock.Unlock()

	return r.deltaInfo(snapshotID, r.sequenceNumber.GetExtendedStart(), r.sequenceNumber.GetExtendedHighest())
}

func (r *RTPStatsReceiver) ToString() string {
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

// ----------------------------------
