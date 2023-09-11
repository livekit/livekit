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
	"errors"
	"fmt"
	"time"

	"github.com/pion/rtcp"

	"github.com/livekit/mediatransportutil"
	"github.com/livekit/protocol/livekit"
)

type senderSnapshot struct {
	snapshot
	extStartSNFromRR  uint64
	packetsLostFromRR uint64
	maxJitterFromRR   float64
}

type RTPStatsSender struct {
	*rtpStatsBase

	extStartSN         uint64
	extHighestSN       uint64
	extHighestSNFromRR uint64

	lastRRTime time.Time
	lastRR     rtcp.ReceptionReport

	extStartTS   uint64
	extHighestTS uint64

	packetsLostFromRR uint64

	jitterFromRR    float64
	maxJitterFromRR float64

	nextSenderSnapshotID uint32
	senderSnapshots      map[uint32]*senderSnapshot
}

func NewRTPStatsSender(params RTPStatsParams) *RTPStatsSender {
	return &RTPStatsSender{
		rtpStatsBase:         newRTPStatsBase(params),
		nextSenderSnapshotID: cFirstSnapshotID,
		senderSnapshots:      make(map[uint32]*senderSnapshot),
	}
}

func (r *RTPStatsSender) Seed(from *RTPStatsSender) {
	r.lock.Lock()
	defer r.lock.Unlock()

	if !r.seed(from.rtpStatsBase) {
		return
	}

	r.extStartSN = from.extStartSN
	r.extHighestSN = from.extHighestSN
	r.extHighestSNFromRR = from.extHighestSNFromRR

	r.lastRRTime = from.lastRRTime
	r.lastRR = from.lastRR

	r.extStartTS = from.extStartTS
	r.extHighestTS = from.extHighestTS

	r.packetsLostFromRR = from.packetsLostFromRR

	r.jitterFromRR = from.jitterFromRR
	r.maxJitterFromRR = from.maxJitterFromRR

	r.nextSenderSnapshotID = from.nextSenderSnapshotID
	for id, ss := range from.senderSnapshots {
		ssCopy := *ss
		r.senderSnapshots[id] = &ssCopy
	}
}

func (r *RTPStatsSender) NewSnapshotId() uint32 {
	r.lock.Lock()
	defer r.lock.Unlock()

	return r.newSnapshotID(r.extStartSN)
}

func (r *RTPStatsSender) NewSenderSnapshotId() uint32 {
	r.lock.Lock()
	defer r.lock.Unlock()

	id := r.nextSenderSnapshotID
	if r.initialized {
		r.senderSnapshots[id] = &senderSnapshot{
			snapshot: snapshot{
				startTime:  time.Now(),
				extStartSN: r.extStartSN,
			},
			extStartSNFromRR: r.extStartSN,
		}
	}
	return id
}

func (r *RTPStatsSender) Update(
	packetTime time.Time,
	extSequenceNumber uint64,
	extTimestamp uint64,
	marker bool,
	hdrSize int,
	payloadSize int,
	paddingSize int,
) {
	r.lock.Lock()
	defer r.lock.Unlock()

	if !r.endTime.IsZero() {
		return
	}

	if !r.initialized {
		if payloadSize == 0 {
			// do not start on a padding only packet
			return
		}

		r.initialized = true

		r.startTime = time.Now()

		r.firstTime = packetTime
		r.highestTime = packetTime

		r.extStartSN = extSequenceNumber
		r.extHighestSN = extSequenceNumber

		r.extStartTS = extTimestamp
		r.extHighestTS = extTimestamp

		// initialize snapshots if any
		for i := uint32(cFirstSnapshotID); i < r.nextSnapshotID; i++ {
			r.snapshots[i] = &snapshot{
				startTime:  r.startTime,
				extStartSN: r.extStartSN,
			}
		}
		for i := uint32(cFirstSnapshotID); i < r.nextSenderSnapshotID; i++ {
			r.senderSnapshots[i] = &senderSnapshot{
				snapshot: snapshot{
					startTime:  r.startTime,
					extStartSN: r.extStartSN,
				},
				extStartSNFromRR: r.extStartSN,
			}
		}

		r.logger.Debugw(
			"rtp sender stream start",
			"startTime", r.startTime.String(),
			"firstTime", r.firstTime.String(),
			"startSN", r.extStartSN,
			"startTS", r.extStartTS,
		)
	}

	pktSize := uint64(hdrSize + payloadSize + paddingSize)
	isDuplicate := false
	gapSN := int64(extSequenceNumber - r.extHighestSN)
	if gapSN <= 0 { // duplicate OR out-of-order
		if payloadSize == 0 && extSequenceNumber < r.extStartSN {
			// do not start on a padding only packet
			return
		}

		if extSequenceNumber < r.extStartSN {
			r.packetsLost += r.extStartSN - extSequenceNumber

			// adjust start of snapshots
			for _, s := range r.snapshots {
				if s.extStartSN == r.extStartSN {
					s.extStartSN = extSequenceNumber
				}
			}
			for _, s := range r.senderSnapshots {
				if s.extStartSN == r.extStartSN {
					s.extStartSN = extSequenceNumber
				}
			}

			r.extStartSN = extSequenceNumber
		}

		if extTimestamp < r.extStartTS {
			r.extStartTS = extTimestamp
		}

		if gapSN != 0 {
			r.packetsOutOfOrder++
		}

		if !r.isSnInfoLost(extSequenceNumber, r.extHighestSN) {
			r.bytesDuplicate += pktSize
			r.headerBytesDuplicate += uint64(hdrSize)
			r.packetsDuplicate++
			isDuplicate = true
		} else {
			r.packetsLost--
			r.setSnInfo(extSequenceNumber, r.extHighestSN, uint16(pktSize), uint16(hdrSize), uint16(payloadSize), marker, true)
		}
	} else { // in-order
		// update gap histogram
		r.updateGapHistogram(int(gapSN))

		// update missing sequence numbers
		r.clearSnInfos(r.extHighestSN+1, extSequenceNumber)
		r.packetsLost += uint64(gapSN - 1)

		r.setSnInfo(extSequenceNumber, r.extHighestSN, uint16(pktSize), uint16(hdrSize), uint16(payloadSize), marker, false)

		if extTimestamp != r.extHighestTS {
			// update only on first packet as same timestamp could be in multiple packets.
			// NOTE: this may not be the first packet with this time stamp if there is packet loss.
			r.highestTime = packetTime
		}
		r.extHighestSN = extSequenceNumber
		r.extHighestTS = extTimestamp
	}

	if !isDuplicate {
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

			jitter := r.updateJitter(extTimestamp, packetTime)
			for _, s := range r.senderSnapshots {
				if jitter > s.maxJitter {
					s.maxJitter = jitter
				}
			}
		}
	}
}

func (r *RTPStatsSender) GetTotalPacketsPrimary() uint64 {
	r.lock.RLock()
	defer r.lock.RUnlock()

	return r.getTotalPacketsPrimary(r.extStartSN, r.extHighestSN)
}

func (r *RTPStatsSender) UpdateFromReceiverReport(rr rtcp.ReceptionReport) (rtt uint32, isRttChanged bool) {
	r.lock.Lock()
	defer r.lock.Unlock()

	if !r.initialized || !r.endTime.IsZero() {
		return
	}

	extHighestSNFromRR := r.extHighestSNFromRR&0xFFFF_FFFF_0000_0000 + uint64(rr.LastSequenceNumber)
	if !r.lastRRTime.IsZero() {
		if (rr.LastSequenceNumber-r.lastRR.LastSequenceNumber) < (1<<31) && rr.LastSequenceNumber < r.lastRR.LastSequenceNumber {
			extHighestSNFromRR += (1 << 32)
		}
	}
	if extHighestSNFromRR < r.extStartSN {
		// it is possible that the `LastSequenceNumber` in the receiver report is before the starting
		// sequence number when dummy packets are used to trigger Pion's OnTrack path.
		return
	}

	var err error
	if r.srNewest != nil {
		rtt, err = mediatransportutil.GetRttMs(&rr, r.srNewest.NTPTimestamp, r.srNewest.At)
		if err == nil {
			isRttChanged = rtt != r.rtt
		} else {
			if !errors.Is(err, mediatransportutil.ErrRttNotLastSenderReport) && !errors.Is(err, mediatransportutil.ErrRttNoLastSenderReport) {
				r.logger.Warnw("error getting rtt", err)
			}
		}
	}

	if r.lastRRTime.IsZero() || r.extHighestSNFromRR <= extHighestSNFromRR {
		r.extHighestSNFromRR = extHighestSNFromRR

		packetsLostFromRR := r.packetsLostFromRR&0xFFFF_FFFF_0000_0000 + uint64(rr.TotalLost)
		if (rr.TotalLost-r.lastRR.TotalLost) < (1<<31) && rr.TotalLost < r.lastRR.TotalLost {
			packetsLostFromRR += (1 << 32)
		}
		r.packetsLostFromRR = packetsLostFromRR

		if isRttChanged {
			r.rtt = rtt
			if rtt > r.maxRtt {
				r.maxRtt = rtt
			}
		}

		r.jitterFromRR = float64(rr.Jitter)
		if r.jitterFromRR > r.maxJitterFromRR {
			r.maxJitterFromRR = r.jitterFromRR
		}

		// update snapshots
		for _, s := range r.snapshots {
			if isRttChanged && rtt > s.maxRtt {
				s.maxRtt = rtt
			}
		}
		for _, s := range r.senderSnapshots {
			if isRttChanged && rtt > s.maxRtt {
				s.maxRtt = rtt
			}

			if r.jitterFromRR > s.maxJitterFromRR {
				s.maxJitterFromRR = r.jitterFromRR
			}
		}

		r.lastRRTime = time.Now()
		r.lastRR = rr
	} else {
		r.logger.Debugw(
			fmt.Sprintf("receiver report potentially out of order, highestSN: existing: %d, received: %d", r.extHighestSNFromRR, rr.LastSequenceNumber),
			"lastRRTime", r.lastRRTime,
			"lastRR", r.lastRR,
			"sinceLastRR", time.Since(r.lastRRTime),
			"receivedRR", rr,
		)
	}
	return
}

func (r *RTPStatsSender) LastReceiverReportTime() time.Time {
	r.lock.RLock()
	defer r.lock.RUnlock()

	return r.lastRRTime
}

func (r *RTPStatsSender) MaybeAdjustFirstPacketTime(ets uint64) {
	r.lock.Lock()
	defer r.lock.Unlock()

	r.maybeAdjustFirstPacketTime(ets, r.extStartTS)
}

func (r *RTPStatsSender) GetExpectedRTPTimestamp(at time.Time) (expectedTSExt uint64, err error) {
	r.lock.RLock()
	defer r.lock.RUnlock()

	if !r.initialized {
		err = errors.New("uninitilaized")
		return
	}

	timeDiff := at.Sub(r.firstTime)
	expectedRTPDiff := timeDiff.Nanoseconds() * int64(r.params.ClockRate) / 1e9
	expectedTSExt = r.extStartTS + uint64(expectedRTPDiff)
	return
}

func (r *RTPStatsSender) GetRtcpSenderReport(ssrc uint32, calculatedClockRate uint32) *rtcp.SenderReport {
	r.lock.Lock()
	defer r.lock.Unlock()

	if !r.initialized {
		return nil
	}

	// construct current time based on monotonic clock
	timeSinceFirst := time.Since(r.firstTime)
	now := r.firstTime.Add(timeSinceFirst)
	nowNTP := mediatransportutil.ToNtpTime(now)

	timeSinceHighest := now.Sub(r.highestTime)
	nowRTPExt := r.extHighestTS + uint64(timeSinceHighest.Nanoseconds()*int64(r.params.ClockRate)/1e9)
	nowRTPExtUsingTime := nowRTPExt
	nowRTP := uint32(nowRTPExt)

	// It is possible that publisher is pacing at a slower rate.
	// That would make `highestTS` to be lagging the RTP time stamp in the RTCP Sender Report from publisher.
	// Check for that using calculated clock rate and use the later time stamp if applicable.
	var nowRTPExtUsingRate uint64
	if calculatedClockRate != 0 {
		nowRTPExtUsingRate = r.extStartTS + uint64(float64(calculatedClockRate)*timeSinceFirst.Seconds())
		if nowRTPExtUsingRate > nowRTPExt {
			nowRTPExt = nowRTPExtUsingRate
			nowRTP = uint32(nowRTPExt)
		}
	}

	if r.srNewest != nil && nowRTPExt < r.srNewest.RTPTimestampExt {
		// If report being generated is behind, use the time difference and
		// clock rate of codec to produce next report.
		//
		// Current report could be behind due to the following
		//  - Publisher pacing
		//  - Due to above, report from publisher side is ahead of packet timestamps.
		//    Note that report will map wall clock to timestamp at capture time and happens before the pacer.
		//  - Pause/Mute followed by resume, some combination of events that could
		//    result in this module not having calculated clock rate of publisher side.
		//  - When the above happens, current will be generated using highestTS which could be behind.
		//    That could end up behind the last report's timestamp in extreme cases
		r.logger.Infow(
			"sending sender report, out-of-order, repairing",
			"prevTSExt", r.srNewest.RTPTimestampExt,
			"prevRTP", r.srNewest.RTPTimestamp,
			"prevNTP", r.srNewest.NTPTimestamp.Time().String(),
			"currTSExt", nowRTPExt,
			"currRTP", nowRTP,
			"currNTP", nowNTP.Time().String(),
			"timeNow", time.Now().String(),
			"firstTime", r.firstTime.String(),
			"timeSinceFirst", timeSinceFirst,
			"highestTime", r.highestTime.String(),
			"timeSinceHighest", timeSinceHighest,
			"nowRTPExtUsingTime", nowRTPExtUsingTime,
			"calculatedClockRate", calculatedClockRate,
			"nowRTPExtUsingRate", nowRTPExtUsingRate,
		)
		ntpDiffSinceLast := nowNTP.Time().Sub(r.srNewest.NTPTimestamp.Time())
		nowRTPExt = r.srNewest.RTPTimestampExt + uint64(ntpDiffSinceLast.Seconds()*float64(r.params.ClockRate))
		nowRTP = uint32(nowRTPExt)
	}

	r.srNewest = &RTCPSenderReportData{
		NTPTimestamp:    nowNTP,
		RTPTimestamp:    nowRTP,
		RTPTimestampExt: nowRTPExt,
		At:              now,
	}
	if r.srFirst == nil {
		r.srFirst = r.srNewest
	}

	return &rtcp.SenderReport{
		SSRC:        ssrc,
		NTPTime:     uint64(nowNTP),
		RTPTime:     nowRTP,
		PacketCount: uint32(r.getTotalPacketsPrimary(r.extStartSN, r.extHighestSN) + r.packetsDuplicate + r.packetsPadding),
		OctetCount:  uint32(r.bytes + r.bytesDuplicate + r.bytesPadding),
	}
}

func (r *RTPStatsSender) DeltaInfo(snapshotID uint32) *RTPDeltaInfo {
	r.lock.Lock()
	defer r.lock.Unlock()

	return r.deltaInfo(snapshotID, r.extStartSN, r.extHighestSN)
}

func (r *RTPStatsSender) DeltaInfoSender(senderSnapshotID uint32) *RTPDeltaInfo {
	r.lock.Lock()
	defer r.lock.Unlock()
	if r.lastRRTime.IsZero() {
		return nil
	}

	then, now := r.getAndResetSenderSnapshot(senderSnapshotID)
	if now == nil || then == nil {
		return nil
	}

	startTime := then.startTime
	endTime := now.startTime

	packetsExpected := now.extStartSNFromRR - then.extStartSNFromRR
	if packetsExpected > cNumSequenceNumbers {
		r.logger.Warnw(
			"too many packets expected in delta (sender)",
			fmt.Errorf("start: %d, end: %d, expected: %d", then.extStartSNFromRR, now.extStartSNFromRR, packetsExpected),
		)
		return nil
	}
	if packetsExpected == 0 {
		// not received RTCP RR (OR) publisher is not producing any data
		return nil
	}

	intervalStats := r.getIntervalStats(then.extStartSNFromRR, now.extStartSNFromRR, r.extHighestSN)
	packetsLost := now.packetsLostFromRR - then.packetsLostFromRR
	if int32(packetsLost) < 0 {
		packetsLost = 0
	}

	if packetsLost > packetsExpected {
		r.logger.Warnw(
			"unexpected number of packets lost",
			fmt.Errorf(
				"start: %d, end: %d, expected: %d, lost: report: %d, interval: %d",
				then.extStartSNFromRR,
				now.extStartSNFromRR,
				packetsExpected,
				now.packetsLostFromRR-then.packetsLostFromRR,
				intervalStats.packetsLost,
			),
		)
		packetsLost = packetsExpected
	}

	// discount jitter from publisher side + internal processing
	maxJitter := then.maxJitterFromRR - then.maxJitter
	if maxJitter < 0.0 {
		maxJitter = 0.0
	}
	maxJitterTime := maxJitter / float64(r.params.ClockRate) * 1e6

	return &RTPDeltaInfo{
		StartTime:            startTime,
		Duration:             endTime.Sub(startTime),
		Packets:              uint32(packetsExpected - intervalStats.packetsPadding),
		Bytes:                intervalStats.bytes,
		HeaderBytes:          intervalStats.headerBytes,
		PacketsDuplicate:     uint32(now.packetsDuplicate - then.packetsDuplicate),
		BytesDuplicate:       now.bytesDuplicate - then.bytesDuplicate,
		HeaderBytesDuplicate: now.headerBytesDuplicate - then.headerBytesDuplicate,
		PacketsPadding:       uint32(intervalStats.packetsPadding),
		BytesPadding:         intervalStats.bytesPadding,
		HeaderBytesPadding:   intervalStats.headerBytesPadding,
		PacketsLost:          uint32(packetsLost),
		PacketsMissing:       uint32(intervalStats.packetsLost),
		PacketsOutOfOrder:    uint32(intervalStats.packetsOutOfOrder),
		Frames:               intervalStats.frames,
		RttMax:               then.maxRtt,
		JitterMax:            maxJitterTime,
		Nacks:                now.nacks - then.nacks,
		Plis:                 now.plis - then.plis,
		Firs:                 now.firs - then.firs,
	}
}

func (r *RTPStatsSender) ToString() string {
	r.lock.RLock()
	defer r.lock.RUnlock()

	return r.toString(
		r.extStartSN, r.extHighestSN, r.extStartTS, r.extHighestTS,
		r.packetsLostFromRR,
		r.jitterFromRR, r.maxJitterFromRR,
	)
}

func (r *RTPStatsSender) ToProto() *livekit.RTPStats {
	r.lock.RLock()
	defer r.lock.RUnlock()

	return r.toProto(
		r.extStartSN, r.extHighestSN, r.extStartTS, r.extHighestTS,
		r.packetsLostFromRR,
		r.jitterFromRR, r.maxJitterFromRR,
	)
}

func (r *RTPStatsSender) getAndResetSenderSnapshot(senderSnapshotID uint32) (*senderSnapshot, *senderSnapshot) {
	if !r.initialized || r.lastRRTime.IsZero() {
		return nil, nil
	}

	then := r.senderSnapshots[senderSnapshotID]
	if then == nil {
		then = &senderSnapshot{
			snapshot: snapshot{
				startTime:  r.startTime,
				extStartSN: r.extStartSN,
			},
			extStartSNFromRR: r.extStartSN,
		}
		r.senderSnapshots[senderSnapshotID] = then
	}

	// snapshot now
	r.senderSnapshots[senderSnapshotID] = &senderSnapshot{
		snapshot: snapshot{
			startTime:            r.lastRRTime,
			extStartSN:           r.extHighestSN + 1,
			packetsDuplicate:     r.packetsDuplicate,
			bytesDuplicate:       r.bytesDuplicate,
			headerBytesDuplicate: r.headerBytesDuplicate,
			nacks:                r.nacks,
			plis:                 r.plis,
			firs:                 r.firs,
			maxJitter:            r.jitter,
			maxRtt:               r.rtt,
		},
		extStartSNFromRR:  r.extHighestSNFromRR + 1,
		packetsLostFromRR: r.packetsLostFromRR,
		maxJitterFromRR:   r.jitterFromRR,
	}
	// make a copy so that it can be used independently
	now := *r.senderSnapshots[senderSnapshotID]

	return then, &now
}

// -------------------------------------------------------------------
