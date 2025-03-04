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
	"fmt"
	"sync"
	"time"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils/mono"
	"go.uber.org/zap/zapcore"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	cGapHistogramNumBins = 101
	cNumSequenceNumbers  = 65536
	cFirstSnapshotID     = 1
)

// -------------------------------------------------------

type RTPDeltaInfoLite struct {
	StartTime         time.Time
	EndTime           time.Time
	Packets           uint32
	Bytes             uint64
	PacketsLost       uint32
	PacketsOutOfOrder uint32
	Nacks             uint32
}

func (r *RTPDeltaInfoLite) MarshalLogObject(e zapcore.ObjectEncoder) error {
	if r == nil {
		return nil
	}

	e.AddTime("StartTime", r.StartTime)
	e.AddTime("EndTime", r.EndTime)
	e.AddUint32("Packets", r.Packets)
	e.AddUint64("Bytes", r.Bytes)
	e.AddUint32("PacketsLost", r.PacketsLost)
	e.AddUint32("PacketsOutOfOrder", r.PacketsOutOfOrder)
	e.AddUint32("Nacks", r.Nacks)
	return nil
}

// -------------------------------------------------------

type snapshotLite struct {
	isValid bool

	startTime int64

	extStartSN uint64
	bytes      uint64

	packetsOutOfOrder uint64

	packetsLost uint64

	nacks uint32
}

func (s *snapshotLite) MarshalLogObject(e zapcore.ObjectEncoder) error {
	if s == nil {
		return nil
	}

	e.AddBool("isValid", s.isValid)
	e.AddTime("startTime", time.Unix(0, s.startTime))
	e.AddUint64("extStartSN", s.extStartSN)
	e.AddUint64("bytes", s.bytes)
	e.AddUint64("packetsOutOfOrder", s.packetsOutOfOrder)
	e.AddUint64("packetsLost", s.packetsLost)
	e.AddUint32("nacks", s.nacks)
	return nil
}

// ------------------------------------------------------------------

type RTPStatsParams struct {
	ClockRate uint32
	IsRTX     bool
	Logger    logger.Logger
}

type rtpStatsBaseLite struct {
	params RTPStatsParams
	logger logger.Logger

	lock sync.RWMutex

	initialized bool

	startTime int64
	endTime   int64

	bytes uint64

	packetsOutOfOrder uint64

	packetsLost uint64

	gapHistogram [cGapHistogramNumBins]uint32

	nacks        uint32
	nackAcks     uint32
	nackMisses   uint32
	nackRepeated uint32

	plis    uint32
	lastPli int64

	nextSnapshotLiteID uint32
	snapshotLites      []snapshotLite
}

func newRTPStatsBaseLite(params RTPStatsParams) *rtpStatsBaseLite {
	return &rtpStatsBaseLite{
		params:             params,
		logger:             params.Logger,
		nextSnapshotLiteID: cFirstSnapshotID,
		snapshotLites:      make([]snapshotLite, 2),
	}
}

func (r *rtpStatsBaseLite) seed(from *rtpStatsBaseLite) bool {
	if from == nil || !from.initialized || r.initialized {
		return false
	}

	r.initialized = from.initialized

	r.startTime = from.startTime
	// do not clone endTime as a non-zero endTime indicates an ended object

	r.bytes = from.bytes

	r.packetsOutOfOrder = from.packetsOutOfOrder

	r.packetsLost = from.packetsLost

	r.gapHistogram = from.gapHistogram

	r.nacks = from.nacks
	r.nackAcks = from.nackAcks
	r.nackMisses = from.nackMisses
	r.nackRepeated = from.nackRepeated

	r.plis = from.plis
	r.lastPli = from.lastPli

	r.nextSnapshotLiteID = from.nextSnapshotLiteID
	r.snapshotLites = make([]snapshotLite, cap(from.snapshotLites))
	copy(r.snapshotLites, from.snapshotLites)
	return true
}

func (r *rtpStatsBaseLite) SetLogger(logger logger.Logger) {
	r.logger = logger
}

func (r *rtpStatsBaseLite) Stop() {
	r.lock.Lock()
	defer r.lock.Unlock()

	r.endTime = mono.UnixNano()
}

func (r *rtpStatsBaseLite) newSnapshotLiteID(extStartSN uint64) uint32 {
	id := r.nextSnapshotLiteID
	r.nextSnapshotLiteID++

	if cap(r.snapshotLites) < int(r.nextSnapshotLiteID-cFirstSnapshotID) {
		snapshotLites := make([]snapshotLite, r.nextSnapshotLiteID-cFirstSnapshotID)
		copy(snapshotLites, r.snapshotLites)
		r.snapshotLites = snapshotLites
	}

	if r.initialized {
		r.snapshotLites[id-cFirstSnapshotID] = initSnapshotLite(mono.UnixNano(), extStartSN)
	}
	return id
}

func (r *rtpStatsBaseLite) IsActive() bool {
	r.lock.RLock()
	defer r.lock.RUnlock()

	return r.initialized && r.endTime == 0
}

func (r *rtpStatsBaseLite) UpdateNack(nackCount uint32) {
	r.lock.Lock()
	defer r.lock.Unlock()

	if r.endTime != 0 {
		return
	}

	r.nacks += nackCount
}

func (r *rtpStatsBaseLite) UpdateNackProcessed(nackAckCount uint32, nackMissCount uint32, nackRepeatedCount uint32) {
	r.lock.Lock()
	defer r.lock.Unlock()

	if r.endTime != 0 {
		return
	}

	r.nackAcks += nackAckCount
	r.nackMisses += nackMissCount
	r.nackRepeated += nackRepeatedCount
}

func (r *rtpStatsBaseLite) CheckAndUpdatePli(throttle int64, force bool) bool {
	r.lock.Lock()
	defer r.lock.Unlock()

	if r.endTime != 0 || (!force && mono.UnixNano()-r.lastPli < throttle) {
		return false
	}
	r.updatePliLocked(1)
	r.updatePliTimeLocked()
	return true
}

func (r *rtpStatsBaseLite) UpdatePliAndTime(pliCount uint32) {
	r.lock.Lock()
	defer r.lock.Unlock()

	if r.endTime != 0 {
		return
	}

	r.updatePliLocked(pliCount)
	r.updatePliTimeLocked()
}

func (r *rtpStatsBaseLite) UpdatePli(pliCount uint32) {
	r.lock.Lock()
	defer r.lock.Unlock()

	if r.endTime != 0 {
		return
	}

	r.updatePliLocked(pliCount)
}

func (r *rtpStatsBaseLite) updatePliLocked(pliCount uint32) {
	r.plis += pliCount
}

func (r *rtpStatsBaseLite) UpdatePliTime() {
	r.lock.Lock()
	defer r.lock.Unlock()

	if r.endTime != 0 {
		return
	}

	r.updatePliTimeLocked()
}

func (r *rtpStatsBaseLite) updatePliTimeLocked() {
	r.lastPli = mono.UnixNano()
}

func (r *rtpStatsBaseLite) LastPli() int64 {
	r.lock.RLock()
	defer r.lock.RUnlock()

	return r.lastPli
}

func (r *rtpStatsBaseLite) getPacketsSeen(extStartSN, extHighestSN uint64) uint64 {
	packetsExpected := getPacketsExpected(extStartSN, extHighestSN)
	if r.packetsLost > packetsExpected {
		// should not happen
		return 0
	}

	return packetsExpected - r.packetsLost
}

func (r *rtpStatsBaseLite) deltaInfoLite(
	snapshotLiteID uint32,
	extStartSN uint64,
	extHighestSN uint64,
) (deltaInfoLite *RTPDeltaInfoLite, err error, loggingFields []interface{}) {
	then, now := r.getAndResetSnapshotLite(snapshotLiteID, extStartSN, extHighestSN)
	if now == nil || then == nil {
		return
	}

	startTime := then.startTime
	endTime := now.startTime

	packetsExpected := uint32(now.extStartSN - then.extStartSN)
	if then.extStartSN > extHighestSN {
		packetsExpected = 0
	}
	if packetsExpected > cNumSequenceNumbers {
		loggingFields = []interface{}{
			"snapshotLiteID", snapshotLiteID,
			"snapshotLiteNow", now,
			"snapshotLiteThen", then,
			"packetsExpected", packetsExpected,
			"duration", time.Duration(endTime - startTime),
		}
		err = errors.New("too many packets expected in delta lite")
		return
	}
	if packetsExpected == 0 {
		deltaInfoLite = &RTPDeltaInfoLite{
			StartTime: time.Unix(0, startTime),
			EndTime:   time.Unix(0, endTime),
		}
		return
	}

	packetsLost := uint32(now.packetsLost - then.packetsLost)
	if int32(packetsLost) < 0 {
		packetsLost = 0
	}
	if packetsLost > packetsExpected {
		loggingFields = []interface{}{
			"snapshotLiteID", snapshotLiteID,
			"snapshotLiteNow", now,
			"snapshotLiteThen", then,
			"packetsExpected", packetsExpected,
			"packetsLost", packetsLost,
			"duration", time.Duration(endTime - startTime),
		}
		err = errors.New("unexpected number of packets lost in delta lite")
	}

	deltaInfoLite = &RTPDeltaInfoLite{
		StartTime:         time.Unix(0, startTime),
		EndTime:           time.Unix(0, endTime),
		Packets:           packetsExpected,
		Bytes:             now.bytes - then.bytes,
		PacketsLost:       packetsLost,
		PacketsOutOfOrder: uint32(now.packetsOutOfOrder - then.packetsOutOfOrder),
		Nacks:             now.nacks - then.nacks,
	}
	return
}

func (r *rtpStatsBaseLite) marshalLogObject(e zapcore.ObjectEncoder, packetsExpected, packetsSeenMinusPadding uint64) (float64, error) {
	if r == nil || !r.initialized {
		return 0, errors.New("not initialized")
	}

	endTime := r.endTime
	if endTime == 0 {
		endTime = mono.UnixNano()
	}
	elapsed := time.Duration(endTime - r.startTime)
	if elapsed == 0 {
		return 0, errors.New("no time elapsed")
	}
	elapsedSeconds := elapsed.Seconds()

	e.AddTime("startTime", time.Unix(0, r.startTime))
	e.AddTime("endTime", time.Unix(0, r.endTime))
	e.AddDuration("elapsed", elapsed)

	e.AddUint64("packetsExpected", packetsExpected)
	e.AddFloat64("packetsExpectedRate", float64(packetsExpected)/elapsedSeconds)
	e.AddUint64("packetsSeenPrimary", packetsSeenMinusPadding)
	e.AddFloat64("packetsSeenPrimaryRate", float64(packetsSeenMinusPadding)/elapsedSeconds)
	e.AddUint64("bytes", r.bytes)
	e.AddFloat64("bitrate", float64(r.bytes)*8.0/elapsedSeconds)

	e.AddUint64("packetsOutOfOrder", r.packetsOutOfOrder)

	e.AddUint64("packetsLost", r.packetsLost)
	e.AddFloat64("packetsLostRate", float64(r.packetsLost)/elapsedSeconds)
	if packetsExpected != 0 {
		e.AddFloat32("packetLostPercentage", float32(r.packetsLost)/float32(packetsExpected)*100.0)
	}

	hasLoss := false
	first := true
	str := "["
	for burst, count := range r.gapHistogram {
		if count == 0 {
			continue
		}

		hasLoss = true

		if !first {
			str += ", "
		}
		first = false
		str += fmt.Sprintf("%d:%d", burst+1, count)
	}
	str += "]"
	if hasLoss {
		e.AddString("gapHistogram", str)
	}

	e.AddUint32("nacks", r.nacks)
	e.AddUint32("nackAcks", r.nackAcks)
	e.AddUint32("nackMisses", r.nackMisses)
	e.AddUint32("nackRepeated", r.nackRepeated)

	e.AddUint32("plis", r.plis)
	e.AddTime("lastPli", time.Unix(0, r.lastPli))
	return elapsedSeconds, nil
}

func (r *rtpStatsBaseLite) toProto(packetsExpected, packetsSeenMinusPadding, packetsLost uint64) *livekit.RTPStats {
	if r.startTime == 0 {
		return nil
	}

	endTime := r.endTime
	if endTime == 0 {
		endTime = mono.UnixNano()
	}
	elapsed := time.Duration(endTime - r.startTime).Seconds()
	if elapsed == 0.0 {
		return nil
	}

	packetRate := float64(packetsSeenMinusPadding) / elapsed
	bitrate := float64(r.bytes) * 8.0 / elapsed

	packetLostRate := float64(packetsLost) / elapsed
	packetLostPercentage := float32(0)
	if packetsExpected != 0 {
		packetLostPercentage = float32(packetsLost) / float32(packetsExpected) * 100.0
	}

	p := &livekit.RTPStats{
		StartTime:            timestamppb.New(time.Unix(0, r.startTime)),
		EndTime:              timestamppb.New(time.Unix(0, endTime)),
		Duration:             elapsed,
		Packets:              uint32(packetsSeenMinusPadding),
		PacketRate:           packetRate,
		Bytes:                r.bytes,
		Bitrate:              bitrate,
		PacketsLost:          uint32(packetsLost),
		PacketLossRate:       packetLostRate,
		PacketLossPercentage: packetLostPercentage,
		PacketsOutOfOrder:    uint32(r.packetsOutOfOrder),
		Nacks:                r.nacks,
		NackAcks:             r.nackAcks,
		NackMisses:           r.nackMisses,
		NackRepeated:         r.nackRepeated,
		Plis:                 r.plis,
		LastPli:              timestamppb.New(time.Unix(0, r.lastPli)),
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
		p.GapHistogram = make(map[int32]uint32, len(r.gapHistogram))
		for i := 0; i < len(r.gapHistogram); i++ {
			if r.gapHistogram[i] == 0 {
				continue
			}

			p.GapHistogram[int32(i+1)] = r.gapHistogram[i]
		}
	}

	return p
}

func (r *rtpStatsBaseLite) getAndResetSnapshotLite(snapshotLiteID uint32, extStartSN uint64, extHighestSN uint64) (*snapshotLite, *snapshotLite) {
	if !r.initialized {
		return nil, nil
	}

	idx := snapshotLiteID - cFirstSnapshotID
	then := r.snapshotLites[idx]
	if !then.isValid {
		then = initSnapshotLite(r.startTime, extStartSN)
		r.snapshotLites[idx] = then
	}

	// snapshot now
	now := r.getSnapshotLite(mono.UnixNano(), extHighestSN+1)
	r.snapshotLites[idx] = now
	return &then, &now
}

func (r *rtpStatsBaseLite) updateGapHistogram(gap int) {
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

func (r *rtpStatsBaseLite) getSnapshotLite(startTime int64, extStartSN uint64) snapshotLite {
	return snapshotLite{
		isValid:           true,
		startTime:         startTime,
		extStartSN:        extStartSN,
		bytes:             r.bytes,
		packetsOutOfOrder: r.packetsOutOfOrder,
		packetsLost:       r.packetsLost,
		nacks:             r.nacks,
	}
}

// ----------------------------------

func initSnapshotLite(startTime int64, extStartSN uint64) snapshotLite {
	return snapshotLite{
		isValid:    true,
		startTime:  startTime,
		extStartSN: extStartSN,
	}
}

func getPacketsExpected(extStartSN, extHighestSN uint64) uint64 {
	return extHighestSN - extStartSN + 1
}

// ----------------------------------
