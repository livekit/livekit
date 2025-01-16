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
	"go.uber.org/zap/zapcore"

	"github.com/livekit/livekit-server/pkg/sfu/utils"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/utils/mono"
)

type RTPFlowStateLite struct {
	IsNotHandled bool

	LossStartInclusive uint64
	LossEndExclusive   uint64

	ExtSequenceNumber uint64
}

func (r *RTPFlowStateLite) MarshalLogObject(e zapcore.ObjectEncoder) error {
	if r == nil {
		return nil
	}

	e.AddBool("IsNotHandled", r.IsNotHandled)
	e.AddUint64("LossStartInclusive", r.LossStartInclusive)
	e.AddUint64("LossEndExclusive", r.LossEndExclusive)
	e.AddUint64("ExtSequenceNumber", r.ExtSequenceNumber)
	return nil
}

// ---------------------------------------------------------------------

type RTPStatsReceiverLite struct {
	*rtpStatsBaseLite

	sequenceNumber *utils.WrapAround[uint16, uint64]
}

func NewRTPStatsReceiverLite(params RTPStatsParams) *RTPStatsReceiverLite {
	return &RTPStatsReceiverLite{
		rtpStatsBaseLite: newRTPStatsBaseLite(params),
		sequenceNumber:   utils.NewWrapAround[uint16, uint64](utils.WrapAroundParams{IsRestartAllowed: false}),
	}
}

func (r *RTPStatsReceiverLite) NewSnapshotLiteId() uint32 {
	r.lock.Lock()
	defer r.lock.Unlock()

	return r.newSnapshotLiteID(r.sequenceNumber.GetExtendedHighest())
}

func (r *RTPStatsReceiverLite) Update(packetTime int64, packetSize int, sequenceNumber uint16) (flowStateLite RTPFlowStateLite) {
	r.lock.Lock()
	defer r.lock.Unlock()

	if r.endTime != 0 {
		flowStateLite.IsNotHandled = true
		return
	}

	var resSN utils.WrapAroundUpdateResult[uint64]
	if !r.initialized {
		r.initialized = true

		r.startTime = mono.UnixNano()

		resSN = r.sequenceNumber.Update(sequenceNumber)

		// initialize lite snapshots if any
		for i := uint32(0); i < r.nextSnapshotLiteID-cFirstSnapshotID; i++ {
			r.snapshotLites[i] = initSnapshotLite(r.startTime, r.sequenceNumber.GetExtendedStart())
		}

		r.logger.Debugw(
			"rtp receiver lite stream start",
			"rtpStats", lockedRTPStatsReceiverLiteLogEncoder{r},
		)
	} else {
		resSN = r.sequenceNumber.Update(sequenceNumber)
		if resSN.IsUnhandled {
			flowStateLite.IsNotHandled = true
			return
		}
	}

	gapSN := int64(resSN.ExtendedVal - resSN.PreExtendedHighest)
	if gapSN <= 0 { // duplicate OR out-of-order
		r.packetsOutOfOrder++ // counting duplicate as out-of-order
		r.packetsLost--
	} else { // in-order
		r.updateGapHistogram(int(gapSN))
		r.packetsLost += uint64(gapSN - 1)

		flowStateLite.LossStartInclusive = resSN.PreExtendedHighest + 1
		flowStateLite.LossEndExclusive = resSN.ExtendedVal
	}
	flowStateLite.ExtSequenceNumber = resSN.ExtendedVal
	r.bytes += uint64(packetSize)
	return
}

func (r *RTPStatsReceiverLite) DeltaInfoLite(snapshotLiteID uint32) *RTPDeltaInfoLite {
	r.lock.Lock()
	defer r.lock.Unlock()

	deltaInfoLite, err, loggingFields := r.deltaInfoLite(
		snapshotLiteID,
		r.sequenceNumber.GetExtendedStart(),
		r.sequenceNumber.GetExtendedHighest(),
	)
	if err != nil {
		r.logger.Infow(err.Error(), append(loggingFields, "rtpStats", lockedRTPStatsReceiverLiteLogEncoder{r})...)
	}

	return deltaInfoLite
}

func (r *RTPStatsReceiverLite) MarshalLogObject(e zapcore.ObjectEncoder) error {
	if r == nil {
		return nil
	}

	r.lock.RLock()
	defer r.lock.RUnlock()

	return lockedRTPStatsReceiverLiteLogEncoder{r}.MarshalLogObject(e)
}

func (r *RTPStatsReceiverLite) ToProto() *livekit.RTPStats {
	r.lock.RLock()
	defer r.lock.RUnlock()

	return r.rtpStatsBaseLite.toProto(r.sequenceNumber.GetExtendedStart(), r.sequenceNumber.GetExtendedHighest(), r.packetsLost)
}

// ----------------------------------

type lockedRTPStatsReceiverLiteLogEncoder struct {
	*RTPStatsReceiverLite
}

func (r lockedRTPStatsReceiverLiteLogEncoder) MarshalLogObject(e zapcore.ObjectEncoder) error {
	if r.RTPStatsReceiverLite == nil {
		return nil
	}

	extStartSN, extHighestSN := r.sequenceNumber.GetExtendedStart(), r.sequenceNumber.GetExtendedHighest()
	if _, err := r.rtpStatsBaseLite.marshalLogObject(
		e,
		getPacketsExpected(extStartSN, extHighestSN),
		getPacketsExpected(extStartSN, extHighestSN),
	); err != nil {
		return err
	}

	e.AddUint64("extStartSN", r.sequenceNumber.GetExtendedStart())
	e.AddUint64("extHighestSN", r.sequenceNumber.GetExtendedHighest())
	return nil
}

// ----------------------------------
