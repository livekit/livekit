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
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/livekit/mediatransportutil"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
)

const (
	cGapHistogramNumBins = 101
	cNumSequenceNumbers  = 65536
	cFirstSnapshotID     = 1

	cFirstPacketTimeAdjustWindow    = 2 * time.Minute
	cFirstPacketTimeAdjustThreshold = 5 * time.Minute
)

// -------------------------------------------------------

func RTPDriftToString(r *livekit.RTPDrift) string {
	if r == nil {
		return "-"
	}

	str := fmt.Sprintf("t: %+v|%+v|%.2fs", r.StartTime.AsTime().Format(time.UnixDate), r.EndTime.AsTime().Format(time.UnixDate), r.Duration)
	str += fmt.Sprintf(", ts: %d|%d|%d", r.StartTimestamp, r.EndTimestamp, r.RtpClockTicks)
	str += fmt.Sprintf(", d: %d|%.2fms", r.DriftSamples, r.DriftMs)
	str += fmt.Sprintf(", cr: %.2f", r.ClockRate)
	return str
}

// -------------------------------------------------------

type RTPDeltaInfo struct {
	StartTime            time.Time
	Duration             time.Duration
	Packets              uint32
	Bytes                uint64
	HeaderBytes          uint64
	PacketsDuplicate     uint32
	BytesDuplicate       uint64
	HeaderBytesDuplicate uint64
	PacketsPadding       uint32
	BytesPadding         uint64
	HeaderBytesPadding   uint64
	PacketsLost          uint32
	PacketsMissing       uint32
	PacketsOutOfOrder    uint32
	Frames               uint32
	RttMax               uint32
	JitterMax            float64
	Nacks                uint32
	Plis                 uint32
	Firs                 uint32
}

type snapshot struct {
	isValid bool

	startTime time.Time

	extStartSN  uint64
	bytes       uint64
	headerBytes uint64

	packetsPadding     uint64
	bytesPadding       uint64
	headerBytesPadding uint64

	packetsDuplicate     uint64
	bytesDuplicate       uint64
	headerBytesDuplicate uint64

	packetsOutOfOrder uint64

	packetsLost uint64

	frames uint32

	nacks uint32
	plis  uint32
	firs  uint32

	maxRtt    uint32
	maxJitter float64
}

// ------------------------------------------------------------------

type RTCPSenderReportData struct {
	RTPTimestamp    uint32
	RTPTimestampExt uint64
	NTPTimestamp    mediatransportutil.NtpTime
	At              time.Time
}

func (r *RTCPSenderReportData) ToString() string {
	if r == nil {
		return ""
	}

	return fmt.Sprintf("ntp: %s, rtp: %d, extRtp: %d, at: %s", r.NTPTimestamp.Time().String(), r.RTPTimestamp, r.RTPTimestampExt, r.At.String())
}

// ------------------------------------------------------------------

type RTPStatsParams struct {
	ClockRate uint32
	Logger    logger.Logger
}

type rtpStatsBase struct {
	params RTPStatsParams
	logger logger.Logger

	lock sync.RWMutex

	initialized bool

	startTime time.Time
	endTime   time.Time

	firstTime   time.Time
	highestTime time.Time

	lastTransit            uint64
	lastJitterExtTimestamp uint64

	bytes                uint64
	headerBytes          uint64
	bytesDuplicate       uint64
	headerBytesDuplicate uint64
	bytesPadding         uint64
	headerBytesPadding   uint64
	packetsDuplicate     uint64
	packetsPadding       uint64

	packetsOutOfOrder uint64

	packetsLost uint64

	frames uint32

	jitter    float64
	maxJitter float64

	gapHistogram [cGapHistogramNumBins]uint32

	nacks        uint32
	nackAcks     uint32
	nackMisses   uint32
	nackRepeated uint32

	plis    uint32
	lastPli time.Time

	layerLockPlis    uint32
	lastLayerLockPli time.Time

	firs    uint32
	lastFir time.Time

	keyFrames    uint32
	lastKeyFrame time.Time

	rtt    uint32
	maxRtt uint32

	srFirst  *RTCPSenderReportData
	srNewest *RTCPSenderReportData

	nextSnapshotID uint32
	snapshots      []snapshot
}

func newRTPStatsBase(params RTPStatsParams) *rtpStatsBase {
	return &rtpStatsBase{
		params:         params,
		logger:         params.Logger,
		nextSnapshotID: cFirstSnapshotID,
		snapshots:      make([]snapshot, 2),
	}
}

func (r *rtpStatsBase) seed(from *rtpStatsBase) bool {
	if from == nil || !from.initialized {
		return false
	}

	r.initialized = from.initialized

	r.startTime = from.startTime
	// do not clone endTime as a non-zero endTime indicates an ended object

	r.firstTime = from.firstTime
	r.highestTime = from.highestTime

	r.lastTransit = from.lastTransit
	r.lastJitterExtTimestamp = from.lastJitterExtTimestamp

	r.bytes = from.bytes
	r.headerBytes = from.headerBytes
	r.bytesDuplicate = from.bytesDuplicate
	r.headerBytesDuplicate = from.headerBytesDuplicate
	r.bytesPadding = from.bytesPadding
	r.headerBytesPadding = from.headerBytesPadding
	r.packetsDuplicate = from.packetsDuplicate
	r.packetsPadding = from.packetsPadding

	r.packetsOutOfOrder = from.packetsOutOfOrder

	r.packetsLost = from.packetsLost

	r.frames = from.frames

	r.jitter = from.jitter
	r.maxJitter = from.maxJitter

	r.gapHistogram = from.gapHistogram

	r.nacks = from.nacks
	r.nackAcks = from.nackAcks
	r.nackMisses = from.nackMisses
	r.nackRepeated = from.nackRepeated

	r.plis = from.plis
	r.lastPli = from.lastPli

	r.layerLockPlis = from.layerLockPlis
	r.lastLayerLockPli = from.lastLayerLockPli

	r.firs = from.firs
	r.lastFir = from.lastFir

	r.keyFrames = from.keyFrames
	r.lastKeyFrame = from.lastKeyFrame

	r.rtt = from.rtt
	r.maxRtt = from.maxRtt

	if from.srFirst != nil {
		srFirst := *from.srFirst
		r.srFirst = &srFirst
	} else {
		r.srFirst = nil
	}
	if from.srNewest != nil {
		srNewest := *from.srNewest
		r.srNewest = &srNewest
	} else {
		r.srNewest = nil
	}

	r.nextSnapshotID = from.nextSnapshotID
	r.snapshots = make([]snapshot, cap(from.snapshots))
	copy(r.snapshots, from.snapshots)
	return true
}

func (r *rtpStatsBase) SetLogger(logger logger.Logger) {
	r.logger = logger
}

func (r *rtpStatsBase) Stop() {
	r.lock.Lock()
	defer r.lock.Unlock()

	r.endTime = time.Now()
}

func (r *rtpStatsBase) newSnapshotID(extStartSN uint64) uint32 {
	id := r.nextSnapshotID
	r.nextSnapshotID++

	if cap(r.snapshots) < int(r.nextSnapshotID-cFirstSnapshotID) {
		snapshots := make([]snapshot, r.nextSnapshotID-cFirstSnapshotID)
		copy(snapshots, r.snapshots)
		r.snapshots = snapshots
	}

	if r.initialized {
		r.snapshots[id-cFirstSnapshotID] = r.initSnapshot(time.Now(), extStartSN)
	}
	return id
}

func (r *rtpStatsBase) IsActive() bool {
	r.lock.RLock()
	defer r.lock.RUnlock()

	return r.initialized && r.endTime.IsZero()
}

func (r *rtpStatsBase) UpdateNack(nackCount uint32) {
	r.lock.Lock()
	defer r.lock.Unlock()

	if !r.endTime.IsZero() {
		return
	}

	r.nacks += nackCount
}

func (r *rtpStatsBase) UpdateNackProcessed(nackAckCount uint32, nackMissCount uint32, nackRepeatedCount uint32) {
	r.lock.Lock()
	defer r.lock.Unlock()

	if !r.endTime.IsZero() {
		return
	}

	r.nackAcks += nackAckCount
	r.nackMisses += nackMissCount
	r.nackRepeated += nackRepeatedCount
}

func (r *rtpStatsBase) UpdatePliAndTime(pliCount uint32) {
	r.lock.Lock()
	defer r.lock.Unlock()

	if !r.endTime.IsZero() {
		return
	}

	r.updatePliLocked(pliCount)
	r.updatePliTimeLocked()
}

func (r *rtpStatsBase) UpdatePli(pliCount uint32) {
	r.lock.Lock()
	defer r.lock.Unlock()

	if !r.endTime.IsZero() {
		return
	}

	r.updatePliLocked(pliCount)
}

func (r *rtpStatsBase) updatePliLocked(pliCount uint32) {
	r.plis += pliCount
}

func (r *rtpStatsBase) UpdatePliTime() {
	r.lock.Lock()
	defer r.lock.Unlock()

	if !r.endTime.IsZero() {
		return
	}

	r.updatePliTimeLocked()
}

func (r *rtpStatsBase) updatePliTimeLocked() {
	r.lastPli = time.Now()
}

func (r *rtpStatsBase) LastPli() time.Time {
	r.lock.RLock()
	defer r.lock.RUnlock()

	return r.lastPli
}

func (r *rtpStatsBase) TimeSinceLastPli() int64 {
	r.lock.RLock()
	defer r.lock.RUnlock()

	return time.Now().UnixNano() - r.lastPli.UnixNano()
}

func (r *rtpStatsBase) UpdateLayerLockPliAndTime(pliCount uint32) {
	r.lock.Lock()
	defer r.lock.Unlock()

	if !r.endTime.IsZero() {
		return
	}

	r.layerLockPlis += pliCount
	r.lastLayerLockPli = time.Now()
}

func (r *rtpStatsBase) UpdateFir(firCount uint32) {
	r.lock.Lock()
	defer r.lock.Unlock()

	if !r.endTime.IsZero() {
		return
	}

	r.firs += firCount
}

func (r *rtpStatsBase) UpdateFirTime() {
	r.lock.Lock()
	defer r.lock.Unlock()

	if !r.endTime.IsZero() {
		return
	}

	r.lastFir = time.Now()
}

func (r *rtpStatsBase) UpdateKeyFrame(kfCount uint32) {
	r.lock.Lock()
	defer r.lock.Unlock()

	if !r.endTime.IsZero() {
		return
	}

	r.keyFrames += kfCount
	r.lastKeyFrame = time.Now()
}

func (r *rtpStatsBase) UpdateRtt(rtt uint32) {
	r.lock.Lock()
	defer r.lock.Unlock()

	if !r.endTime.IsZero() {
		return
	}

	r.rtt = rtt
	if rtt > r.maxRtt {
		r.maxRtt = rtt
	}

	for i := uint32(0); i < r.nextSnapshotID-cFirstSnapshotID; i++ {
		s := &r.snapshots[i]
		if rtt > s.maxRtt {
			s.maxRtt = rtt
		}
	}
}

func (r *rtpStatsBase) GetRtt() uint32 {
	r.lock.RLock()
	defer r.lock.RUnlock()

	return r.rtt
}

func (r *rtpStatsBase) maybeAdjustFirstPacketTime(ts uint32, startTS uint32) {
	if time.Since(r.startTime) > cFirstPacketTimeAdjustWindow {
		return
	}

	// for some time after the start, adjust time of first packet.
	// Helps improve accuracy of expected timestamp calculation.
	// Adjusting only one way, i. e. if the first sample experienced
	// abnormal delay (maybe due to pacing or maybe due to queuing
	// in some network element along the way), push back first time
	// to an earlier instance.
	samplesDiff := int32(ts - startTS)
	if samplesDiff < 0 {
		// out-of-order, skip
		return
	}

	samplesDuration := time.Duration(float64(samplesDiff) / float64(r.params.ClockRate) * float64(time.Second))
	timeSinceFirst := time.Since(r.firstTime)
	now := r.firstTime.Add(timeSinceFirst)
	firstTime := now.Add(-samplesDuration)
	if firstTime.Before(r.firstTime) {
		r.logger.Debugw(
			"adjusting first packet time",
			"startTime", r.startTime.String(),
			"nowTime", now.String(),
			"before", r.firstTime.String(),
			"after", firstTime.String(),
			"adjustment", r.firstTime.Sub(firstTime).String(),
			"nowTS", ts,
			"startTS", startTS,
		)
		if r.firstTime.Sub(firstTime) > cFirstPacketTimeAdjustThreshold {
			r.logger.Infow("first packet time adjustment too big, ignoring",
				"startTime", r.startTime.String(),
				"nowTime", now.String(),
				"before", r.firstTime.String(),
				"after", firstTime.String(),
				"adjustment", r.firstTime.Sub(firstTime).String(),
				"nowTS", ts,
				"startTS", startTS,
			)
		} else {
			r.firstTime = firstTime
		}
	}
}

func (r *rtpStatsBase) getTotalPacketsPrimary(extStartSN, extHighestSN uint64) uint64 {
	packetsExpected := extHighestSN - extStartSN + 1
	if r.packetsLost > packetsExpected {
		// should not happen
		return 0
	}

	packetsSeen := packetsExpected - r.packetsLost
	if r.packetsPadding > packetsSeen {
		return 0
	}

	return packetsSeen - r.packetsPadding
}

func (r *rtpStatsBase) deltaInfo(snapshotID uint32, extStartSN uint64, extHighestSN uint64) *RTPDeltaInfo {
	then, now := r.getAndResetSnapshot(snapshotID, extStartSN, extHighestSN)
	if now == nil || then == nil {
		return nil
	}

	startTime := then.startTime
	endTime := now.startTime

	packetsExpected := now.extStartSN - then.extStartSN
	if packetsExpected > cNumSequenceNumbers {
		r.logger.Infow(
			"too many packets expected in delta",
			"startSN", then.extStartSN,
			"endSN", now.extStartSN,
			"packetsExpected", packetsExpected,
			"startTime", startTime,
			"endTime", endTime,
			"duration", endTime.Sub(startTime).String(),
		)
		return nil
	}
	if packetsExpected == 0 {
		return &RTPDeltaInfo{
			StartTime: startTime,
			Duration:  endTime.Sub(startTime),
		}
	}

	packetsLost := uint32(now.packetsLost - then.packetsLost)
	if int32(packetsLost) < 0 {
		packetsLost = 0
	}

	// padding packets delta could be higher than expected due to out-of-order padding packets
	packetsPadding := now.packetsPadding - then.packetsPadding
	if packetsExpected < packetsPadding {
		r.logger.Infow("padding packets more than expected", "packetsExpected", packetsExpected, "packetsPadding", packetsPadding)
		packetsExpected = 0
	} else {
		packetsExpected -= packetsPadding
	}

	return &RTPDeltaInfo{
		StartTime:            startTime,
		Duration:             endTime.Sub(startTime),
		Packets:              uint32(packetsExpected),
		Bytes:                now.bytes - then.bytes,
		HeaderBytes:          now.headerBytes - then.headerBytes,
		PacketsDuplicate:     uint32(now.packetsDuplicate - then.packetsDuplicate),
		BytesDuplicate:       now.bytesDuplicate - then.bytesDuplicate,
		HeaderBytesDuplicate: now.headerBytesDuplicate - then.headerBytesDuplicate,
		PacketsPadding:       uint32(packetsPadding),
		BytesPadding:         now.bytesPadding - then.bytesPadding,
		HeaderBytesPadding:   now.headerBytesPadding - then.headerBytesPadding,
		PacketsLost:          packetsLost,
		PacketsOutOfOrder:    uint32(now.packetsOutOfOrder - then.packetsOutOfOrder),
		Frames:               now.frames - then.frames,
		RttMax:               then.maxRtt,
		JitterMax:            then.maxJitter / float64(r.params.ClockRate) * 1e6,
		Nacks:                now.nacks - then.nacks,
		Plis:                 now.plis - then.plis,
		Firs:                 now.firs - then.firs,
	}
}

func (r *rtpStatsBase) toString(
	extStartSN, extHighestSN, extStartTS, extHighestTS uint64,
	packetsLost uint64,
	jitter, maxJitter float64,
) string {
	p := r.toProto(
		extStartSN, extHighestSN, extStartTS, extHighestTS,
		packetsLost,
		jitter, maxJitter,
	)
	if p == nil {
		return ""
	}

	expectedPackets := extHighestSN - extStartSN + 1
	expectedPacketRate := float64(expectedPackets) / p.Duration

	str := fmt.Sprintf("t: %+v|%+v|%.2fs", p.StartTime.AsTime().Format(time.UnixDate), p.EndTime.AsTime().Format(time.UnixDate), p.Duration)

	str += fmt.Sprintf(", sn: %d|%d", extStartSN, extHighestSN)
	str += fmt.Sprintf(", ep: %d|%.2f/s", expectedPackets, expectedPacketRate)

	str += fmt.Sprintf(", p: %d|%.2f/s", p.Packets, p.PacketRate)
	str += fmt.Sprintf(", l: %d|%.1f/s|%.2f%%", p.PacketsLost, p.PacketLossRate, p.PacketLossPercentage)
	str += fmt.Sprintf(", b: %d|%.1fbps|%d", p.Bytes, p.Bitrate, p.HeaderBytes)
	str += fmt.Sprintf(", f: %d|%.1f/s / %d|%+v", p.Frames, p.FrameRate, p.KeyFrames, p.LastKeyFrame.AsTime().Format(time.UnixDate))

	str += fmt.Sprintf(", d: %d|%.2f/s", p.PacketsDuplicate, p.PacketDuplicateRate)
	str += fmt.Sprintf(", bd: %d|%.1fbps|%d", p.BytesDuplicate, p.BitrateDuplicate, p.HeaderBytesDuplicate)

	str += fmt.Sprintf(", pp: %d|%.2f/s", p.PacketsPadding, p.PacketPaddingRate)
	str += fmt.Sprintf(", bp: %d|%.1fbps|%d", p.BytesPadding, p.BitratePadding, p.HeaderBytesPadding)

	str += fmt.Sprintf(", o: %d", p.PacketsOutOfOrder)

	str += fmt.Sprintf(", c: %d, j: %d(%.1fus)|%d(%.1fus)", r.params.ClockRate, uint32(jitter), p.JitterCurrent, uint32(maxJitter), p.JitterMax)

	if len(p.GapHistogram) != 0 {
		first := true
		str += ", gh:["
		for burst, count := range p.GapHistogram {
			if !first {
				str += ", "
			}
			first = false
			str += fmt.Sprintf("%d:%d", burst, count)
		}
		str += "]"
	}

	str += ", n:"
	str += fmt.Sprintf("%d|%d|%d|%d", p.Nacks, p.NackAcks, p.NackMisses, p.NackRepeated)

	str += ", pli:"
	str += fmt.Sprintf("%d|%+v / %d|%+v",
		p.Plis, p.LastPli.AsTime().Format(time.UnixDate),
		p.LayerLockPlis, p.LastLayerLockPli.AsTime().Format(time.UnixDate),
	)

	str += ", fir:"
	str += fmt.Sprintf("%d|%+v", p.Firs, p.LastFir.AsTime().Format(time.UnixDate))

	str += ", rtt(ms):"
	str += fmt.Sprintf("%d|%d", p.RttCurrent, p.RttMax)

	str += fmt.Sprintf(", pd: %s, rd: %s", RTPDriftToString(p.PacketDrift), RTPDriftToString(p.ReportDrift))
	return str
}

func (r *rtpStatsBase) toProto(
	extStartSN, extHighestSN, extStartTS, extHighestTS uint64,
	packetsLost uint64,
	jitter, maxJitter float64,
) *livekit.RTPStats {
	if r.startTime.IsZero() {
		return nil
	}

	endTime := r.endTime
	if endTime.IsZero() {
		endTime = time.Now()
	}
	elapsed := endTime.Sub(r.startTime).Seconds()
	if elapsed == 0.0 {
		return nil
	}

	packets := r.getTotalPacketsPrimary(extStartSN, extHighestSN)
	packetRate := float64(packets) / elapsed
	bitrate := float64(r.bytes) * 8.0 / elapsed

	frameRate := float64(r.frames) / elapsed

	packetsExpected := extHighestSN - extStartSN + 1
	packetLostRate := float64(packetsLost) / elapsed
	packetLostPercentage := float32(packetsLost) / float32(packetsExpected) * 100.0

	packetDuplicateRate := float64(r.packetsDuplicate) / elapsed
	bitrateDuplicate := float64(r.bytesDuplicate) * 8.0 / elapsed

	packetPaddingRate := float64(r.packetsPadding) / elapsed
	bitratePadding := float64(r.bytesPadding) * 8.0 / elapsed

	jitterTime := jitter / float64(r.params.ClockRate) * 1e6
	maxJitterTime := maxJitter / float64(r.params.ClockRate) * 1e6

	packetDrift, reportDrift := r.getDrift(extStartTS, extHighestTS)

	p := &livekit.RTPStats{
		StartTime:            timestamppb.New(r.startTime),
		EndTime:              timestamppb.New(endTime),
		Duration:             elapsed,
		Packets:              uint32(packets),
		PacketRate:           packetRate,
		Bytes:                r.bytes,
		HeaderBytes:          r.headerBytes,
		Bitrate:              bitrate,
		PacketsLost:          uint32(packetsLost),
		PacketLossRate:       packetLostRate,
		PacketLossPercentage: packetLostPercentage,
		PacketsDuplicate:     uint32(r.packetsDuplicate),
		PacketDuplicateRate:  packetDuplicateRate,
		BytesDuplicate:       r.bytesDuplicate,
		HeaderBytesDuplicate: r.headerBytesDuplicate,
		BitrateDuplicate:     bitrateDuplicate,
		PacketsPadding:       uint32(r.packetsPadding),
		PacketPaddingRate:    packetPaddingRate,
		BytesPadding:         r.bytesPadding,
		HeaderBytesPadding:   r.headerBytesPadding,
		BitratePadding:       bitratePadding,
		PacketsOutOfOrder:    uint32(r.packetsOutOfOrder),
		Frames:               r.frames,
		FrameRate:            frameRate,
		KeyFrames:            r.keyFrames,
		LastKeyFrame:         timestamppb.New(r.lastKeyFrame),
		JitterCurrent:        jitterTime,
		JitterMax:            maxJitterTime,
		Nacks:                r.nacks,
		NackAcks:             r.nackAcks,
		NackMisses:           r.nackMisses,
		NackRepeated:         r.nackRepeated,
		Plis:                 r.plis,
		LastPli:              timestamppb.New(r.lastPli),
		LayerLockPlis:        r.layerLockPlis,
		LastLayerLockPli:     timestamppb.New(r.lastLayerLockPli),
		Firs:                 r.firs,
		LastFir:              timestamppb.New(r.lastFir),
		RttCurrent:           r.rtt,
		RttMax:               r.maxRtt,
		PacketDrift:          packetDrift,
		ReportDrift:          reportDrift,
	}

	gapsPresent := false
	for i := 0; i < len(r.gapHistogram); i++ {
		if r.gapHistogram[i] == 0 {
			continue
		}

		gapsPresent = true
		break
	}

	if gapsPresent {
		p.GapHistogram = make(map[int32]uint32, cGapHistogramNumBins)
		for i := 0; i < len(r.gapHistogram); i++ {
			if r.gapHistogram[i] == 0 {
				continue
			}

			p.GapHistogram[int32(i+1)] = r.gapHistogram[i]
		}
	}

	return p
}

func (r *rtpStatsBase) updateJitter(ets uint64, packetTime time.Time) float64 {
	// Do not update jitter on multiple packets of same frame.
	// All packets of a frame have the same time stamp.
	// NOTE: This does not protect against using more than one packet of the same frame
	//       if packets arrive out-of-order. For example,
	//          p1f1 -> p1f2 -> p2f1
	//       In this case, p2f1 (packet 2, frame 1) will still be used in jitter calculation
	//       although it is the second packet of a frame because of out-of-order receival.
	if r.lastJitterExtTimestamp != ets {
		timeSinceFirst := packetTime.Sub(r.firstTime)
		packetTimeRTP := uint64(timeSinceFirst.Nanoseconds() * int64(r.params.ClockRate) / 1e9)
		transit := packetTimeRTP - ets

		if r.lastTransit != 0 {
			d := int64(transit - r.lastTransit)
			if d < 0 {
				d = -d
			}
			r.jitter += (float64(d) - r.jitter) / 16
			if r.jitter > r.maxJitter {
				r.maxJitter = r.jitter
			}

			for i := uint32(0); i < r.nextSnapshotID-cFirstSnapshotID; i++ {
				s := &r.snapshots[i]
				if r.jitter > s.maxJitter {
					s.maxJitter = r.jitter
				}
			}
		}

		r.lastTransit = transit
		r.lastJitterExtTimestamp = ets
	}
	return r.jitter
}

func (r *rtpStatsBase) getAndResetSnapshot(snapshotID uint32, extStartSN uint64, extHighestSN uint64) (*snapshot, *snapshot) {
	if !r.initialized {
		return nil, nil
	}

	idx := snapshotID - cFirstSnapshotID
	then := r.snapshots[idx]
	if !then.isValid {
		then = r.initSnapshot(r.startTime, extStartSN)
		r.snapshots[idx] = then
	}

	// snapshot now
	now := r.getSnapshot(time.Now(), extHighestSN+1)
	r.snapshots[idx] = now
	return &then, &now
}

func (r *rtpStatsBase) getDrift(extStartTS, extHighestTS uint64) (packetDrift *livekit.RTPDrift, reportDrift *livekit.RTPDrift) {
	if !r.firstTime.IsZero() {
		elapsed := r.highestTime.Sub(r.firstTime)
		rtpClockTicks := extHighestTS - extStartTS
		driftSamples := int64(rtpClockTicks - uint64(elapsed.Nanoseconds()*int64(r.params.ClockRate)/1e9))
		if elapsed.Seconds() > 0.0 {
			packetDrift = &livekit.RTPDrift{
				StartTime:      timestamppb.New(r.firstTime),
				EndTime:        timestamppb.New(r.highestTime),
				Duration:       elapsed.Seconds(),
				StartTimestamp: extStartTS,
				EndTimestamp:   extHighestTS,
				RtpClockTicks:  rtpClockTicks,
				DriftSamples:   driftSamples,
				DriftMs:        (float64(driftSamples) * 1000) / float64(r.params.ClockRate),
				ClockRate:      float64(rtpClockTicks) / elapsed.Seconds(),
			}
		}
	}

	if r.srFirst != nil && r.srNewest != nil && r.srFirst.RTPTimestamp != r.srNewest.RTPTimestamp {
		elapsed := r.srNewest.NTPTimestamp.Time().Sub(r.srFirst.NTPTimestamp.Time())
		rtpClockTicks := r.srNewest.RTPTimestampExt - r.srFirst.RTPTimestampExt
		driftSamples := int64(rtpClockTicks - uint64(elapsed.Nanoseconds()*int64(r.params.ClockRate)/1e9))
		if elapsed.Seconds() > 0.0 {
			reportDrift = &livekit.RTPDrift{
				StartTime:      timestamppb.New(r.srFirst.NTPTimestamp.Time()),
				EndTime:        timestamppb.New(r.srNewest.NTPTimestamp.Time()),
				Duration:       elapsed.Seconds(),
				StartTimestamp: r.srFirst.RTPTimestampExt,
				EndTimestamp:   r.srNewest.RTPTimestampExt,
				RtpClockTicks:  rtpClockTicks,
				DriftSamples:   driftSamples,
				DriftMs:        (float64(driftSamples) * 1000) / float64(r.params.ClockRate),
				ClockRate:      float64(rtpClockTicks) / elapsed.Seconds(),
			}
		}
	}
	return
}

func (r *rtpStatsBase) updateGapHistogram(gap int) {
	if gap < 2 {
		return
	}

	missing := gap - 1
	if missing > len(r.gapHistogram) {
		r.gapHistogram[len(r.gapHistogram)-1]++
	} else {
		r.gapHistogram[missing-1]++
	}
}

func (r *rtpStatsBase) initSnapshot(startTime time.Time, extStartSN uint64) snapshot {
	return snapshot{
		isValid:    true,
		startTime:  startTime,
		extStartSN: extStartSN,
	}
}

func (r *rtpStatsBase) getSnapshot(startTime time.Time, extStartSN uint64) snapshot {
	return snapshot{
		isValid:              true,
		startTime:            startTime,
		extStartSN:           extStartSN,
		bytes:                r.bytes,
		headerBytes:          r.headerBytes,
		packetsPadding:       r.packetsPadding,
		bytesPadding:         r.bytesPadding,
		headerBytesPadding:   r.headerBytesPadding,
		packetsDuplicate:     r.packetsDuplicate,
		bytesDuplicate:       r.bytesDuplicate,
		headerBytesDuplicate: r.headerBytesDuplicate,
		packetsLost:          r.packetsLost,
		packetsOutOfOrder:    r.packetsOutOfOrder,
		frames:               r.frames,
		nacks:                r.nacks,
		plis:                 r.plis,
		firs:                 r.firs,
		maxRtt:               r.rtt,
		maxJitter:            r.jitter,
	}
}

// ----------------------------------

func AggregateRTPStats(statsList []*livekit.RTPStats) *livekit.RTPStats {
	if len(statsList) == 0 {
		return nil
	}

	startTime := time.Time{}
	endTime := time.Time{}

	packets := uint32(0)
	bytes := uint64(0)
	headerBytes := uint64(0)
	packetsLost := uint32(0)
	packetsDuplicate := uint32(0)
	bytesDuplicate := uint64(0)
	headerBytesDuplicate := uint64(0)
	packetsPadding := uint32(0)
	bytesPadding := uint64(0)
	headerBytesPadding := uint64(0)
	packetsOutOfOrder := uint32(0)
	frames := uint32(0)
	keyFrames := uint32(0)
	lastKeyFrame := time.Time{}
	jitter := 0.0
	maxJitter := float64(0)
	gapHistogram := make(map[int32]uint32, cGapHistogramNumBins)
	nacks := uint32(0)
	nackAcks := uint32(0)
	nackMisses := uint32(0)
	nackRepeated := uint32(0)
	plis := uint32(0)
	lastPli := time.Time{}
	layerLockPlis := uint32(0)
	lastLayerLockPli := time.Time{}
	firs := uint32(0)
	lastFir := time.Time{}
	rtt := uint32(0)
	maxRtt := uint32(0)

	for _, stats := range statsList {
		if startTime.IsZero() || startTime.After(stats.StartTime.AsTime()) {
			startTime = stats.StartTime.AsTime()
		}

		if endTime.IsZero() || endTime.Before(stats.EndTime.AsTime()) {
			endTime = stats.EndTime.AsTime()
		}

		packets += stats.Packets
		bytes += stats.Bytes
		headerBytes += stats.HeaderBytes

		packetsLost += stats.PacketsLost

		packetsDuplicate += stats.PacketsDuplicate
		bytesDuplicate += stats.BytesDuplicate
		headerBytesDuplicate += stats.HeaderBytesDuplicate

		packetsPadding += stats.PacketsPadding
		bytesPadding += stats.BytesPadding
		headerBytesPadding += stats.HeaderBytesPadding

		packetsOutOfOrder += stats.PacketsOutOfOrder

		frames += stats.Frames

		keyFrames += stats.KeyFrames
		if lastKeyFrame.IsZero() || lastKeyFrame.Before(stats.LastKeyFrame.AsTime()) {
			lastKeyFrame = stats.LastKeyFrame.AsTime()
		}

		jitter += stats.JitterCurrent
		if stats.JitterMax > maxJitter {
			maxJitter = stats.JitterMax
		}

		for burst, count := range stats.GapHistogram {
			gapHistogram[burst] += count
		}

		nacks += stats.Nacks
		nackAcks += stats.NackAcks
		nackMisses += stats.NackMisses
		nackRepeated += stats.NackRepeated

		plis += stats.Plis
		if lastPli.IsZero() || lastPli.Before(stats.LastPli.AsTime()) {
			lastPli = stats.LastPli.AsTime()
		}

		layerLockPlis += stats.LayerLockPlis
		if lastLayerLockPli.IsZero() || lastLayerLockPli.Before(stats.LastLayerLockPli.AsTime()) {
			lastLayerLockPli = stats.LastLayerLockPli.AsTime()
		}

		firs += stats.Firs
		if lastFir.IsZero() || lastPli.Before(stats.LastFir.AsTime()) {
			lastFir = stats.LastFir.AsTime()
		}

		rtt += stats.RttCurrent
		if stats.RttMax > maxRtt {
			maxRtt = stats.RttMax
		}
	}

	if endTime.IsZero() {
		endTime = time.Now()
	}
	elapsed := endTime.Sub(startTime).Seconds()

	packetLostRate := float64(packetsLost) / elapsed
	packetLostPercentage := float32(packetsLost) / (float32(packets) + float32(packetsLost)) * 100.0

	packetRate := float64(packets) / elapsed
	packetDuplicateRate := float64(packetsDuplicate) / elapsed
	packetPaddingRate := float64(packetsPadding) / elapsed

	bitrate := float64(bytes) * 8.0 / elapsed
	bitrateDuplicate := float64(bytesDuplicate) * 8.0 / elapsed
	bitratePadding := float64(bytesPadding) * 8.0 / elapsed

	frameRate := float64(frames) / elapsed

	return &livekit.RTPStats{
		StartTime:            timestamppb.New(startTime),
		EndTime:              timestamppb.New(endTime),
		Duration:             elapsed,
		Packets:              packets,
		PacketRate:           packetRate,
		Bytes:                bytes,
		HeaderBytes:          headerBytes,
		Bitrate:              bitrate,
		PacketsLost:          packetsLost,
		PacketLossRate:       packetLostRate,
		PacketLossPercentage: packetLostPercentage,
		PacketsDuplicate:     packetsDuplicate,
		PacketDuplicateRate:  packetDuplicateRate,
		BytesDuplicate:       bytesDuplicate,
		HeaderBytesDuplicate: headerBytesDuplicate,
		BitrateDuplicate:     bitrateDuplicate,
		PacketsPadding:       packetsPadding,
		PacketPaddingRate:    packetPaddingRate,
		BytesPadding:         bytesPadding,
		HeaderBytesPadding:   headerBytesPadding,
		BitratePadding:       bitratePadding,
		PacketsOutOfOrder:    packetsOutOfOrder,
		Frames:               frames,
		FrameRate:            frameRate,
		KeyFrames:            keyFrames,
		LastKeyFrame:         timestamppb.New(lastKeyFrame),
		JitterCurrent:        jitter / float64(len(statsList)),
		JitterMax:            maxJitter,
		GapHistogram:         gapHistogram,
		Nacks:                nacks,
		NackAcks:             nackAcks,
		NackMisses:           nackMisses,
		NackRepeated:         nackRepeated,
		Plis:                 plis,
		LastPli:              timestamppb.New(lastPli),
		LayerLockPlis:        layerLockPlis,
		LastLayerLockPli:     timestamppb.New(lastLayerLockPli),
		Firs:                 firs,
		LastFir:              timestamppb.New(lastFir),
		RttCurrent:           rtt / uint32(len(statsList)),
		RttMax:               maxRtt,
		// no aggregation for drift calculations
	}
}

func AggregateRTPDeltaInfo(deltaInfoList []*RTPDeltaInfo) *RTPDeltaInfo {
	if len(deltaInfoList) == 0 {
		return nil
	}

	startTime := time.Time{}
	endTime := time.Time{}

	packets := uint32(0)
	bytes := uint64(0)
	headerBytes := uint64(0)

	packetsDuplicate := uint32(0)
	bytesDuplicate := uint64(0)
	headerBytesDuplicate := uint64(0)

	packetsPadding := uint32(0)
	bytesPadding := uint64(0)
	headerBytesPadding := uint64(0)

	packetsLost := uint32(0)
	packetsMissing := uint32(0)
	packetsOutOfOrder := uint32(0)

	frames := uint32(0)

	maxRtt := uint32(0)
	maxJitter := float64(0)

	nacks := uint32(0)
	plis := uint32(0)
	firs := uint32(0)

	for _, deltaInfo := range deltaInfoList {
		if deltaInfo == nil {
			continue
		}

		if startTime.IsZero() || startTime.After(deltaInfo.StartTime) {
			startTime = deltaInfo.StartTime
		}

		endedAt := deltaInfo.StartTime.Add(deltaInfo.Duration)
		if endTime.IsZero() || endTime.Before(endedAt) {
			endTime = endedAt
		}

		packets += deltaInfo.Packets
		bytes += deltaInfo.Bytes
		headerBytes += deltaInfo.HeaderBytes

		packetsDuplicate += deltaInfo.PacketsDuplicate
		bytesDuplicate += deltaInfo.BytesDuplicate
		headerBytesDuplicate += deltaInfo.HeaderBytesDuplicate

		packetsPadding += deltaInfo.PacketsPadding
		bytesPadding += deltaInfo.BytesPadding
		headerBytesPadding += deltaInfo.HeaderBytesPadding

		packetsLost += deltaInfo.PacketsLost
		packetsMissing += deltaInfo.PacketsMissing
		packetsOutOfOrder += deltaInfo.PacketsOutOfOrder

		frames += deltaInfo.Frames

		if deltaInfo.RttMax > maxRtt {
			maxRtt = deltaInfo.RttMax
		}

		if deltaInfo.JitterMax > maxJitter {
			maxJitter = deltaInfo.JitterMax
		}

		nacks += deltaInfo.Nacks
		plis += deltaInfo.Plis
		firs += deltaInfo.Firs
	}
	if startTime.IsZero() || endTime.IsZero() {
		return nil
	}

	return &RTPDeltaInfo{
		StartTime:            startTime,
		Duration:             endTime.Sub(startTime),
		Packets:              packets,
		Bytes:                bytes,
		HeaderBytes:          headerBytes,
		PacketsDuplicate:     packetsDuplicate,
		BytesDuplicate:       bytesDuplicate,
		HeaderBytesDuplicate: headerBytesDuplicate,
		PacketsPadding:       packetsPadding,
		BytesPadding:         bytesPadding,
		HeaderBytesPadding:   headerBytesPadding,
		PacketsLost:          packetsLost,
		PacketsMissing:       packetsMissing,
		PacketsOutOfOrder:    packetsOutOfOrder,
		Frames:               frames,
		RttMax:               maxRtt,
		JitterMax:            maxJitter,
		Nacks:                nacks,
		Plis:                 plis,
		Firs:                 firs,
	}
}

func DiffToTrafficStats(before, after *livekit.RTPStats) *livekit.TrafficStats {
	if after == nil {
		return nil
	}

	startTime := after.StartTime
	if before != nil {
		startTime = before.EndTime
	}

	if before == nil {
		return &livekit.TrafficStats{
			StartTime: startTime,
			EndTime:   after.EndTime,
			Packets:   after.Packets,
			Bytes:     after.Bytes + after.BytesDuplicate + after.BytesPadding,
		}
	}

	return &livekit.TrafficStats{
		StartTime: startTime,
		EndTime:   after.EndTime,
		Packets:   after.Packets - before.Packets,
		Bytes:     (after.Bytes + after.BytesDuplicate + after.BytesPadding) - (before.Bytes + before.BytesDuplicate + before.BytesPadding),
	}
}

// -------------------------------------------------------------------
