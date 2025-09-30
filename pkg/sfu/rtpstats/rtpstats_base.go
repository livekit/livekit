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
	"errors"
	"time"

	"go.uber.org/zap/zapcore"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/livekit/mediatransportutil"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/utils"
	"github.com/livekit/protocol/utils/mono"
)

const (
	cFirstPacketTimeAdjustWindow    = 2 * time.Minute
	cFirstPacketTimeAdjustThreshold = 15 * 1e9

	cSequenceNumberLargeJumpThreshold = 100
)

// -------------------------------------------------------

type RTPDeltaInfo struct {
	StartTime            time.Time
	EndTime              time.Time
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
	NackRepeated         uint32
	Plis                 uint32
	Firs                 uint32
}

func (r *RTPDeltaInfo) MarshalLogObject(e zapcore.ObjectEncoder) error {
	if r == nil {
		return nil
	}

	e.AddTime("StartTime", r.StartTime)
	e.AddTime("EndTime", r.EndTime)
	e.AddUint32("Packets", r.Packets)
	e.AddUint64("Bytes", r.Bytes)
	e.AddUint64("HeaderBytes", r.HeaderBytes)
	e.AddUint32("PacketsDuplicate", r.PacketsDuplicate)
	e.AddUint64("BytesDuplicate", r.BytesDuplicate)
	e.AddUint64("HeaderBytesDuplicate", r.HeaderBytesDuplicate)
	e.AddUint32("PacketsPadding", r.PacketsPadding)
	e.AddUint64("BytesPadding", r.BytesPadding)
	e.AddUint64("HeaderBytesPadding", r.HeaderBytesPadding)
	e.AddUint32("PacketsLost", r.PacketsLost)
	e.AddUint32("PacketsMissing", r.PacketsMissing)
	e.AddUint32("PacketsOutOfOrder", r.PacketsOutOfOrder)
	e.AddUint32("Frames", r.Frames)
	e.AddUint32("RttMax", r.RttMax)
	e.AddFloat64("JitterMax", r.JitterMax)
	e.AddUint32("Nacks", r.Nacks)
	e.AddUint32("NackRepeated", r.NackRepeated)
	e.AddUint32("Plis", r.Plis)
	e.AddUint32("Firs", r.Firs)
	return nil
}

// -------------------------------------------------------

type snapshot struct {
	snapshotLite

	headerBytes uint64

	packetsDuplicate     uint64
	bytesDuplicate       uint64
	headerBytesDuplicate uint64

	packetsPadding     uint64
	bytesPadding       uint64
	headerBytesPadding uint64

	frames uint32

	plis uint32
	firs uint32

	maxRtt    uint32
	maxJitter float64
}

func (s *snapshot) MarshalLogObject(e zapcore.ObjectEncoder) error {
	if s == nil {
		return nil
	}

	e.AddObject("snapshotLite", &s.snapshotLite)
	e.AddUint64("headerBytes", s.headerBytes)
	e.AddUint64("packetsDuplicate", s.packetsDuplicate)
	e.AddUint64("bytesDuplicate", s.bytesDuplicate)
	e.AddUint64("headerBytesDuplicate", s.headerBytesDuplicate)
	e.AddUint64("packetsPadding", s.packetsPadding)
	e.AddUint64("bytesPadding", s.bytesPadding)
	e.AddUint64("headerBytesPadding", s.headerBytesPadding)
	e.AddUint32("frames", s.frames)
	e.AddUint32("plis", s.plis)
	e.AddUint32("firs", s.firs)
	e.AddUint32("maxRtt", s.maxRtt)
	e.AddFloat64("maxJitter", s.maxJitter)
	return nil
}

func (s *snapshot) maybeUpdateMaxRTT(rtt uint32) {
	if rtt > s.maxRtt {
		s.maxRtt = rtt
	}
}

func (s *snapshot) maybeUpdateMaxJitter(jitter float64) {
	if jitter > s.maxJitter {
		s.maxJitter = jitter
	}
}

// ------------------------------------------------------------------

type wrappedRTPDriftLogger struct {
	*livekit.RTPDrift
}

func (w wrappedRTPDriftLogger) MarshalLogObject(e zapcore.ObjectEncoder) error {
	rd := w.RTPDrift
	if rd == nil {
		return nil
	}

	e.AddTime("StartTime", rd.StartTime.AsTime())
	e.AddTime("EndTime", rd.EndTime.AsTime())
	e.AddFloat64("Duration", rd.Duration)
	e.AddUint64("StartTimestamp", rd.StartTimestamp)
	e.AddUint64("EndTimestamp", rd.EndTimestamp)
	e.AddUint64("RtpClockTicks", rd.RtpClockTicks)
	e.AddInt64("DriftSamples", rd.DriftSamples)
	e.AddFloat64("DriftMs", rd.DriftMs)
	e.AddFloat64("ClockRate", rd.ClockRate)
	return nil
}

// ------------------------------------------------------------------

type WrappedRTCPSenderReportStateLogger struct {
	*livekit.RTCPSenderReportState
}

func (w WrappedRTCPSenderReportStateLogger) MarshalLogObject(e zapcore.ObjectEncoder) error {
	rsrs := w.RTCPSenderReportState
	if rsrs == nil {
		return nil
	}

	e.AddUint32("RtpTimestamp", rsrs.RtpTimestamp)
	e.AddUint64("RtpTimestampExt", rsrs.RtpTimestampExt)
	e.AddTime("NtpTimestamp", mediatransportutil.NtpTime(rsrs.NtpTimestamp).Time())
	e.AddTime("At", time.Unix(0, rsrs.At))
	e.AddTime("AtAdjusted", time.Unix(0, rsrs.AtAdjusted))
	e.AddUint32("Packets", rsrs.Packets)
	e.AddUint64("Octets", rsrs.Octets)
	return nil
}

func RTCPSenderReportPropagationDelay(rsrs *livekit.RTCPSenderReportState, passThrough bool) time.Duration {
	if passThrough {
		return 0
	}

	return time.Unix(0, rsrs.AtAdjusted).Sub(mediatransportutil.NtpTime(rsrs.NtpTimestamp).Time())
}

// ------------------------------------------------------------------

type rtpStatsBase struct {
	*rtpStatsBaseLite

	firstTime           int64
	firstTimeAdjustment time.Duration
	highestTime         int64

	lastTransit            uint64
	lastJitterExtTimestamp uint64

	headerBytes uint64

	packetsDuplicate     uint64
	bytesDuplicate       uint64
	headerBytesDuplicate uint64

	packetsPadding     uint64
	bytesPadding       uint64
	headerBytesPadding uint64

	frames uint32

	jitter    float64
	maxJitter float64

	firs    uint32
	lastFir time.Time

	keyFrames    uint32
	lastKeyFrame time.Time

	rtt    uint32
	maxRtt uint32

	srFirst  *livekit.RTCPSenderReportState
	srNewest *livekit.RTCPSenderReportState

	nextSnapshotID uint32
	snapshots      []snapshot
}

func newRTPStatsBase(params RTPStatsParams) *rtpStatsBase {
	return &rtpStatsBase{
		rtpStatsBaseLite: newRTPStatsBaseLite(params),
		nextSnapshotID:   cFirstSnapshotID,
		snapshots:        make([]snapshot, 2),
	}
}

func (r *rtpStatsBase) seed(from *rtpStatsBase) bool {
	if !r.rtpStatsBaseLite.seed(from.rtpStatsBaseLite) {
		return false
	}

	r.firstTime = from.firstTime
	r.firstTimeAdjustment = from.firstTimeAdjustment
	r.highestTime = from.highestTime

	r.lastTransit = from.lastTransit
	r.lastJitterExtTimestamp = from.lastJitterExtTimestamp

	r.headerBytes = from.headerBytes

	r.packetsDuplicate = from.packetsDuplicate
	r.bytesDuplicate = from.bytesDuplicate
	r.headerBytesDuplicate = from.headerBytesDuplicate

	r.packetsPadding = from.packetsPadding
	r.bytesPadding = from.bytesPadding
	r.headerBytesPadding = from.headerBytesPadding

	r.frames = from.frames

	r.jitter = from.jitter
	r.maxJitter = from.maxJitter

	r.firs = from.firs
	r.lastFir = from.lastFir

	r.keyFrames = from.keyFrames
	r.lastKeyFrame = from.lastKeyFrame

	r.rtt = from.rtt
	r.maxRtt = from.maxRtt

	r.srFirst = utils.CloneProto(from.srFirst)
	r.srNewest = utils.CloneProto(from.srNewest)

	r.nextSnapshotID = from.nextSnapshotID
	r.snapshots = make([]snapshot, cap(from.snapshots))
	copy(r.snapshots, from.snapshots)
	return true
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
		r.snapshots[id-cFirstSnapshotID] = initSnapshot(mono.UnixNano(), extStartSN)
	}
	return id
}

func (r *rtpStatsBase) UpdateFir(firCount uint32) {
	r.lock.Lock()
	defer r.lock.Unlock()

	if r.endTime != 0 {
		return
	}

	r.firs += firCount
}

func (r *rtpStatsBase) UpdateFirTime() {
	r.lock.Lock()
	defer r.lock.Unlock()

	if r.endTime != 0 {
		return
	}

	r.lastFir = time.Now()
}

func (r *rtpStatsBase) UpdateKeyFrame(kfCount uint32) {
	r.lock.Lock()
	defer r.lock.Unlock()

	if r.endTime != 0 {
		return
	}

	r.keyFrames += kfCount
	r.lastKeyFrame = time.Now()
}

func (r *rtpStatsBase) KeyFrame() (uint32, time.Time) {
	r.lock.RLock()
	defer r.lock.RUnlock()

	return r.keyFrames, r.lastKeyFrame
}

func (r *rtpStatsBase) UpdateRtt(rtt uint32) {
	r.lock.Lock()
	defer r.lock.Unlock()

	if r.endTime != 0 {
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

func (r *rtpStatsBase) maybeAdjustFirstPacketTime(
	srData *livekit.RTCPSenderReportState,
	tsOffset uint64,
	extStartTS uint64,
) (adjustment int64, err error, loggingFields []interface{}) {
	nowNano := mono.UnixNano()
	if time.Duration(nowNano-r.startTime) > cFirstPacketTimeAdjustWindow {
		return
	}

	// for some time after the start, adjust time of first packet.
	// Helps improve accuracy of expected timestamp calculation.
	// Adjusting only one way, i. e. if the first sample experienced
	// abnormal delay (maybe due to pacing or maybe due to queuing
	// in some network element along the way), push back first time
	// to an earlier instance.
	timeSinceReceive := time.Duration(nowNano - srData.AtAdjusted)
	extNowTS := srData.RtpTimestampExt - tsOffset + uint64(timeSinceReceive.Nanoseconds()*int64(r.params.ClockRate)/1e9)
	samplesDiff := int64(extNowTS - extStartTS)
	if samplesDiff < 0 {
		// out-of-order, skip
		return
	}

	samplesDuration := time.Duration(float64(samplesDiff) / float64(r.params.ClockRate) * float64(time.Second))
	timeSinceFirst := time.Duration(nowNano - r.firstTime)
	now := r.firstTime + timeSinceFirst.Nanoseconds()
	firstTime := now - samplesDuration.Nanoseconds()
	adjustment = r.firstTime - firstTime

	getFields := func() []interface{} {
		return []interface{}{
			"startTime", time.Unix(0, r.startTime),
			"nowTime", time.Unix(0, now),
			"before", time.Unix(0, r.firstTime),
			"after", time.Unix(0, firstTime),
			"adjustment", time.Duration(adjustment),
			"firstTimeAdjustment", r.firstTimeAdjustment,
			"extNowTS", extNowTS,
			"extStartTS", extStartTS,
			"srData", WrappedRTCPSenderReportStateLogger{srData},
			"tsOffset", tsOffset,
			"timeSinceReceive", timeSinceReceive,
			"timeSinceFirst", timeSinceFirst,
			"samplesDiff", samplesDiff,
			"samplesDuration", samplesDuration,
		}
	}

	if firstTime < r.firstTime {
		if adjustment > cFirstPacketTimeAdjustThreshold {
			err = errors.New("adjusting first packet time, too big, ignoring")
			loggingFields = getFields()
		} else {
			r.firstTimeAdjustment += time.Duration(adjustment)
			r.logger.Debugw("adjusting first packet time", getFields()...)
			r.firstTime = firstTime
		}
	}

	return
}

func (r *rtpStatsBase) getPacketsSeenMinusPadding(extStartSN, extHighestSN uint64) uint64 {
	packetsSeen := r.getPacketsSeen(extStartSN, extHighestSN)
	if r.packetsPadding > packetsSeen {
		return 0
	}

	return packetsSeen - r.packetsPadding
}

func (r *rtpStatsBase) getPacketsSeenPlusDuplicates(extStartSN, extHighestSN uint64) uint64 {
	return r.getPacketsSeen(extStartSN, extHighestSN) + r.packetsDuplicate
}

func (r *rtpStatsBase) deltaInfo(
	snapshotID uint32,
	extStartSN uint64,
	extHighestSN uint64,
) (deltaInfo *RTPDeltaInfo, err error, loggingFields []interface{}) {
	then, now := r.getAndResetSnapshot(snapshotID, extStartSN, extHighestSN)
	if now == nil || then == nil {
		return
	}

	startTime := then.startTime
	endTime := now.startTime

	packetsExpected := now.extStartSN - then.extStartSN
	if then.extStartSN > extHighestSN {
		packetsExpected = 0
	}
	if packetsExpected > cNumSequenceNumbers {
		loggingFields = []interface{}{
			"snapshotID", snapshotID,
			"snapshotNow", now,
			"snapshotThen", then,
			"duration", time.Duration(endTime - startTime),
			"packetsExpected", packetsExpected,
		}
		err = errors.New("too many packets expected in delta")
		return
	}
	if packetsExpected == 0 {
		deltaInfo = &RTPDeltaInfo{
			StartTime: time.Unix(0, startTime),
			EndTime:   time.Unix(0, endTime),
		}
		return
	}

	packetsLost := uint32(now.packetsLost - then.packetsLost)
	if int32(packetsLost) < 0 {
		packetsLost = 0
	}

	// padding packets delta could be higher than expected due to out-of-order padding packets
	packetsPadding := now.packetsPadding - then.packetsPadding
	if packetsExpected < packetsPadding {
		loggingFields = []interface{}{
			"snapshotID", snapshotID,
			"snapshotNow", now,
			"snapshotThen", then,
			"duration", time.Duration(endTime - startTime),
			"packetsExpected", packetsExpected,
			"packetsPadding", packetsPadding,
			"packetsLost", packetsLost,
		}
		err = errors.New("padding packets more than expected")
		packetsExpected = 0
	} else {
		packetsExpected -= packetsPadding
	}

	deltaInfo = &RTPDeltaInfo{
		StartTime:            time.Unix(0, startTime),
		EndTime:              time.Unix(0, endTime),
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
	return
}

func (r *rtpStatsBase) marshalLogObject(
	e zapcore.ObjectEncoder,
	packetsExpected, packetsSeenMinusPadding uint64,
	extStartTS, extHighestTS uint64,
) (float64, error) {
	if r == nil {
		return 0, nil
	}

	elapsedSeconds, err := r.rtpStatsBaseLite.marshalLogObject(e, packetsExpected, packetsSeenMinusPadding)
	if err != nil {
		return 0, err
	}

	e.AddTime("firstTime", time.Unix(0, r.firstTime))
	e.AddDuration("firstTimeAdjustment", r.firstTimeAdjustment)
	e.AddTime("highestTime", time.Unix(0, r.highestTime))

	e.AddUint64("headerBytes", r.headerBytes)

	e.AddUint64("packetsDuplicate", r.packetsDuplicate)
	e.AddFloat64("packetsDuplicateRate", float64(r.packetsDuplicate)/elapsedSeconds)
	e.AddUint64("bytesDuplicate", r.bytesDuplicate)
	e.AddFloat64("bitrateDuplicate", float64(r.bytesDuplicate)*8.0/elapsedSeconds)
	e.AddUint64("headerBytesDuplicate", r.headerBytesDuplicate)

	e.AddUint64("packetsPadding", r.packetsPadding)
	e.AddFloat64("packetsPaddingRate", float64(r.packetsPadding)/elapsedSeconds)
	e.AddUint64("bytesPadding", r.bytesPadding)
	e.AddFloat64("bitratePadding", float64(r.bytesPadding)*8.0/elapsedSeconds)
	e.AddUint64("headerBytesPadding", r.headerBytesPadding)

	e.AddUint32("frames", r.frames)
	e.AddFloat64("frameRate", float64(r.frames)/elapsedSeconds)

	e.AddFloat64("jitter", r.jitter)
	e.AddFloat64("maxJitter", r.maxJitter)

	e.AddUint32("firs", r.firs)
	e.AddTime("lastFir", r.lastFir)

	e.AddUint32("keyFrames", r.keyFrames)
	e.AddTime("lastKeyFrame", r.lastKeyFrame)

	e.AddUint32("rtt", r.rtt)
	e.AddUint32("maxRtt", r.maxRtt)

	e.AddObject("srFirst", WrappedRTCPSenderReportStateLogger{r.srFirst})
	e.AddObject("srNewest", WrappedRTCPSenderReportStateLogger{r.srNewest})

	packetDrift, ntpReportDrift, receivedReportDrift, rebasedReportDrift := r.getDrift(extStartTS, extHighestTS)
	e.AddObject("packetDrift", wrappedRTPDriftLogger{packetDrift})
	e.AddObject("ntpReportDrift", wrappedRTPDriftLogger{ntpReportDrift})
	e.AddObject("receivedReportDrift", wrappedRTPDriftLogger{receivedReportDrift})
	e.AddObject("rebasedReportDrift", wrappedRTPDriftLogger{rebasedReportDrift})
	return elapsedSeconds, nil
}

func (r *rtpStatsBase) toProto(
	packetsExpected, packetsSeenMinusPadding, packetsLost uint64,
	extStartTS, extHighestTS uint64,
	jitter, maxJitter float64,
) *livekit.RTPStats {
	p := r.rtpStatsBaseLite.toProto(packetsExpected, packetsSeenMinusPadding, packetsLost)
	if p == nil {
		return nil
	}

	p.HeaderBytes = r.headerBytes

	p.PacketsDuplicate = uint32(r.packetsDuplicate)
	p.PacketDuplicateRate = float64(r.packetsDuplicate) / p.Duration
	p.BytesDuplicate = r.bytesDuplicate
	p.BitrateDuplicate = float64(r.bytesDuplicate) * 8.0 / p.Duration
	p.HeaderBytesDuplicate = r.headerBytesDuplicate

	p.PacketsPadding = uint32(r.packetsPadding)
	p.PacketPaddingRate = float64(r.packetsPadding) / p.Duration
	p.BytesPadding = r.bytesPadding
	p.BitratePadding = float64(r.bytesPadding) * 8.0 / p.Duration
	p.HeaderBytesPadding = r.headerBytesPadding

	p.Frames = r.frames
	p.FrameRate = float64(r.frames) / p.Duration

	p.KeyFrames = r.keyFrames
	p.LastKeyFrame = timestamppb.New(r.lastKeyFrame)

	p.JitterCurrent = jitter / float64(r.params.ClockRate) * 1e6
	p.JitterMax = maxJitter / float64(r.params.ClockRate) * 1e6

	p.Firs = r.firs
	p.LastFir = timestamppb.New(r.lastFir)

	p.RttCurrent = r.rtt
	p.RttMax = r.maxRtt

	p.PacketDrift, p.NtpReportDrift, p.ReceivedReportDrift, p.RebasedReportDrift = r.getDrift(extStartTS, extHighestTS)
	return p
}

func (r *rtpStatsBase) updateJitter(ets uint64, packetTime int64) float64 {
	// Do not update jitter on multiple packets of same frame.
	// All packets of a frame have the same time stamp.
	// NOTE: This does not protect against using more than one packet of the same frame
	//       if packets arrive out-of-order. For example,
	//          p1f1 -> p1f2 -> p2f1
	//       In this case, p2f1 (packet 2, frame 1) will still be used in jitter calculation
	//       although it is the second packet of a frame because of out-of-order receival.
	if r.lastJitterExtTimestamp != ets {
		timeSinceFirst := packetTime - r.firstTime
		packetTimeRTP := uint64(timeSinceFirst * int64(r.params.ClockRate) / 1e9)
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
				r.snapshots[i].maybeUpdateMaxJitter(r.jitter)
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
		then = initSnapshot(r.startTime, extStartSN)
		r.snapshots[idx] = then
	}

	// snapshot now
	now := r.getSnapshot(mono.UnixNano(), extHighestSN+1)
	r.snapshots[idx] = now
	return &then, &now
}

func (r *rtpStatsBase) getDrift(extStartTS, extHighestTS uint64) (
	packetDrift *livekit.RTPDrift,
	ntpReportDrift *livekit.RTPDrift,
	receivedReportDrift *livekit.RTPDrift,
	rebasedReportDrift *livekit.RTPDrift,
) {
	if r.firstTime != 0 {
		elapsed := r.highestTime - r.firstTime
		rtpClockTicks := extHighestTS - extStartTS
		driftSamples := int64(rtpClockTicks - uint64(elapsed*int64(r.params.ClockRate)/1e9))
		if elapsed > 0 {
			elapsedSeconds := time.Duration(elapsed).Seconds()
			packetDrift = &livekit.RTPDrift{
				StartTime:      timestamppb.New(time.Unix(0, r.firstTime)),
				EndTime:        timestamppb.New(time.Unix(0, r.highestTime)),
				Duration:       elapsedSeconds,
				StartTimestamp: extStartTS,
				EndTimestamp:   extHighestTS,
				RtpClockTicks:  rtpClockTicks,
				DriftSamples:   driftSamples,
				DriftMs:        (float64(driftSamples) * 1000) / float64(r.params.ClockRate),
				ClockRate:      float64(rtpClockTicks) / elapsedSeconds,
			}
		}
	}

	if r.srFirst != nil && r.srNewest != nil && r.srFirst.RtpTimestamp != r.srNewest.RtpTimestamp {
		rtpClockTicks := r.srNewest.RtpTimestampExt - r.srFirst.RtpTimestampExt

		elapsed := mediatransportutil.NtpTime(r.srNewest.NtpTimestamp).Time().Sub(mediatransportutil.NtpTime(r.srFirst.NtpTimestamp).Time())
		if elapsed.Seconds() > 0.0 {
			driftSamples := int64(rtpClockTicks - uint64(elapsed.Nanoseconds()*int64(r.params.ClockRate)/1e9))
			ntpReportDrift = &livekit.RTPDrift{
				StartTime:      timestamppb.New(mediatransportutil.NtpTime(r.srFirst.NtpTimestamp).Time()),
				EndTime:        timestamppb.New(mediatransportutil.NtpTime(r.srNewest.NtpTimestamp).Time()),
				Duration:       elapsed.Seconds(),
				StartTimestamp: r.srFirst.RtpTimestampExt,
				EndTimestamp:   r.srNewest.RtpTimestampExt,
				RtpClockTicks:  rtpClockTicks,
				DriftSamples:   driftSamples,
				DriftMs:        (float64(driftSamples) * 1000) / float64(r.params.ClockRate),
				ClockRate:      float64(rtpClockTicks) / elapsed.Seconds(),
			}
		}

		elapsed = time.Duration(r.srNewest.At - r.srFirst.At)
		if elapsed.Seconds() > 0.0 {
			driftSamples := int64(rtpClockTicks - uint64(elapsed.Nanoseconds()*int64(r.params.ClockRate)/1e9))
			receivedReportDrift = &livekit.RTPDrift{
				StartTime:      timestamppb.New(time.Unix(0, r.srFirst.At)),
				EndTime:        timestamppb.New(time.Unix(0, r.srNewest.At)),
				Duration:       elapsed.Seconds(),
				StartTimestamp: r.srFirst.RtpTimestampExt,
				EndTimestamp:   r.srNewest.RtpTimestampExt,
				RtpClockTicks:  rtpClockTicks,
				DriftSamples:   driftSamples,
				DriftMs:        (float64(driftSamples) * 1000) / float64(r.params.ClockRate),
				ClockRate:      float64(rtpClockTicks) / elapsed.Seconds(),
			}
		}

		elapsed = time.Duration(r.srNewest.AtAdjusted - r.srFirst.AtAdjusted)
		if elapsed.Seconds() > 0.0 {
			driftSamples := int64(rtpClockTicks - uint64(elapsed.Nanoseconds()*int64(r.params.ClockRate)/1e9))
			rebasedReportDrift = &livekit.RTPDrift{
				StartTime:      timestamppb.New(time.Unix(0, r.srFirst.AtAdjusted)),
				EndTime:        timestamppb.New(time.Unix(0, r.srNewest.AtAdjusted)),
				Duration:       elapsed.Seconds(),
				StartTimestamp: r.srFirst.RtpTimestampExt,
				EndTimestamp:   r.srNewest.RtpTimestampExt,
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

func (r *rtpStatsBase) getSnapshot(startTime int64, extStartSN uint64) snapshot {
	return snapshot{
		snapshotLite:         r.getSnapshotLite(startTime, extStartSN),
		headerBytes:          r.headerBytes,
		packetsDuplicate:     r.packetsDuplicate,
		bytesDuplicate:       r.bytesDuplicate,
		headerBytesDuplicate: r.headerBytesDuplicate,
		packetsPadding:       r.packetsPadding,
		bytesPadding:         r.bytesPadding,
		headerBytesPadding:   r.headerBytesPadding,
		frames:               r.frames,
		plis:                 r.plis,
		firs:                 r.firs,
		maxRtt:               r.rtt,
		maxJitter:            r.jitter,
	}
}

// ----------------------------------

func initSnapshot(startTime int64, extStartSN uint64) snapshot {
	return snapshot{
		snapshotLite: initSnapshotLite(startTime, extStartSN),
	}
}

func AggregateRTPStats(statsList []*livekit.RTPStats) *livekit.RTPStats {
	return utils.AggregateRTPStats(statsList, cGapHistogramNumBins)
}

func AggregateRTPDeltaInfo(deltaInfoList []*RTPDeltaInfo) *RTPDeltaInfo {
	if len(deltaInfoList) == 0 {
		return nil
	}

	startTime := int64(0)
	endTime := int64(0)

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

		if startTime == 0 || startTime > deltaInfo.StartTime.UnixNano() {
			startTime = deltaInfo.StartTime.UnixNano()
		}

		if endTime == 0 || endTime < deltaInfo.EndTime.UnixNano() {
			endTime = deltaInfo.EndTime.UnixNano()
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
	if startTime == 0 || endTime == 0 {
		return nil
	}

	return &RTPDeltaInfo{
		StartTime:            time.Unix(0, startTime),
		EndTime:              time.Unix(0, endTime),
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

func ReconcileRTPStatsWithRTX(primaryStats *livekit.RTPStats, rtxStats *livekit.RTPStats) *livekit.RTPStats {
	if primaryStats == nil || rtxStats == nil {
		return primaryStats
	}

	primaryStats.PacketsDuplicate += rtxStats.Packets
	primaryStats.PacketDuplicateRate = float64(primaryStats.PacketsDuplicate) / primaryStats.Duration

	primaryStats.BytesDuplicate += rtxStats.Bytes
	primaryStats.HeaderBytesDuplicate += rtxStats.HeaderBytes
	primaryStats.BitrateDuplicate = float64(primaryStats.BytesDuplicate) * 8.0 / primaryStats.Duration

	primaryStats.PacketsPadding += rtxStats.PacketsPadding
	primaryStats.PacketPaddingRate = float64(primaryStats.PacketsPadding) / primaryStats.Duration

	primaryStats.BytesPadding += rtxStats.BytesPadding
	primaryStats.HeaderBytesPadding += rtxStats.HeaderBytesPadding
	primaryStats.BitratePadding = float64(primaryStats.BytesPadding) * 8.0 / primaryStats.Duration

	// RTX non-padding packets are responses to NACKs, that should discount packets lost,
	lossAdjustment := rtxStats.Packets - rtxStats.PacketsLost - primaryStats.NackRepeated
	if int32(lossAdjustment) < 0 {
		lossAdjustment = 0
	}
	if lossAdjustment >= primaryStats.PacketsLost {
		primaryStats.PacketsLost = 0
	} else {
		primaryStats.PacketsLost -= lossAdjustment
	}
	primaryStats.PacketLossRate = float64(primaryStats.PacketsLost) / primaryStats.Duration
	primaryStats.PacketLossPercentage = float32(primaryStats.PacketsLost) / float32(primaryStats.Packets+primaryStats.PacketsPadding+primaryStats.PacketsLost) * 100.0
	return primaryStats
}

func ReconcileRTPDeltaInfoWithRTX(primaryDeltaInfo *RTPDeltaInfo, rtxDeltaInfo *RTPDeltaInfo) *RTPDeltaInfo {
	if primaryDeltaInfo == nil || rtxDeltaInfo == nil {
		return primaryDeltaInfo
	}

	primaryDeltaInfo.PacketsDuplicate += rtxDeltaInfo.Packets

	primaryDeltaInfo.BytesDuplicate += rtxDeltaInfo.Bytes
	primaryDeltaInfo.HeaderBytesDuplicate += rtxDeltaInfo.HeaderBytes

	primaryDeltaInfo.PacketsPadding += rtxDeltaInfo.PacketsPadding

	primaryDeltaInfo.BytesPadding += rtxDeltaInfo.BytesPadding
	primaryDeltaInfo.HeaderBytesPadding += rtxDeltaInfo.HeaderBytesPadding

	// RTX non-padding packets are responses to NACKs, that should discount packets lost
	lossAdjustment := rtxDeltaInfo.Packets - rtxDeltaInfo.PacketsLost - primaryDeltaInfo.NackRepeated
	if int32(lossAdjustment) < 0 {
		lossAdjustment = 0
	}
	if lossAdjustment >= primaryDeltaInfo.PacketsLost {
		primaryDeltaInfo.PacketsLost = 0
	} else {
		primaryDeltaInfo.PacketsLost -= lossAdjustment
	}
	return primaryDeltaInfo
}

// -------------------------------------------------------------------
