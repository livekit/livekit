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
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/utils/mono"
	"go.uber.org/zap/zapcore"
)

type RTPStatsSenderLite struct {
	*rtpStatsBaseLite

	extStartSN   uint64
	extHighestSN uint64
}

func NewRTPStatsSenderLite(params RTPStatsParams) *RTPStatsSenderLite {
	return &RTPStatsSenderLite{
		rtpStatsBaseLite: newRTPStatsBaseLite(params),
	}
}

func (r *RTPStatsSenderLite) Update(packetTime int64, packetSize int, extSequenceNumber uint64) {
	r.lock.Lock()
	defer r.lock.Unlock()

	if r.endTime != 0 {
		return
	}

	if !r.initialized {
		r.initialized = true

		r.startTime = mono.UnixNano()

		r.extStartSN = extSequenceNumber
		r.extHighestSN = extSequenceNumber - 1

		r.logger.Debugw(
			"rtp sender lite stream start",
			"rtpStats", lockedRTPStatsSenderLiteLogEncoder{r},
		)
	}

	gapSN := int64(extSequenceNumber - r.extHighestSN)
	if gapSN <= 0 { // duplicate OR out-of-order
		r.packetsOutOfOrder++ // counting duplicate as out-of-order
		r.packetsLost--
	} else { // in-order
		r.updateGapHistogram(int(gapSN))
		r.packetsLost += uint64(gapSN - 1)

		r.extHighestSN = extSequenceNumber
	}

	r.bytes += uint64(packetSize)
}

func (r *RTPStatsSenderLite) MarshalLogObject(e zapcore.ObjectEncoder) error {
	if r == nil {
		return nil
	}

	r.lock.RLock()
	defer r.lock.RUnlock()

	return lockedRTPStatsSenderLiteLogEncoder{r}.MarshalLogObject(e)
}

func (r *RTPStatsSenderLite) ToProto() *livekit.RTPStats {
	r.lock.RLock()
	defer r.lock.RUnlock()

	return r.rtpStatsBaseLite.toProto(r.extStartSN, r.extHighestSN, r.packetsLost)
}

func (r *RTPStatsSenderLite) ExtHighestSequenceNumber() uint64 {
	r.lock.RLock()
	defer r.lock.RUnlock()

	return r.extHighestSN
}

// -------------------------------------------------------------------

type lockedRTPStatsSenderLiteLogEncoder struct {
	*RTPStatsSenderLite
}

func (r lockedRTPStatsSenderLiteLogEncoder) MarshalLogObject(e zapcore.ObjectEncoder) error {
	if r.RTPStatsSenderLite == nil {
		return nil
	}

	if _, err := r.rtpStatsBaseLite.marshalLogObject(
		e,
		getPacketsExpected(r.extStartSN, r.extHighestSN),
		getPacketsExpected(r.extStartSN, r.extHighestSN),
	); err != nil {
		return err
	}

	e.AddUint64("extStartSN", r.extStartSN)
	e.AddUint64("extHighestSN", r.extHighestSN)
	return nil
}

// -------------------------------------------------------------------
