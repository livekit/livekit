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

package rtc

import (
	"sync"
	"time"

	"github.com/livekit/livekit-server/pkg/rtc/datatrack"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils/mono"
)

type dataTrackStatsParams struct {
	Logger logger.Logger
}

type dataTrackStats struct {
	params dataTrackStatsParams

	lock                  sync.Mutex
	startTime             int64
	endTime               int64
	highestSequenceNumber uint16
	numPackets            int
	numPacketsLost        int
	numPacketsOutOfOrder  int
	numFrames             int // count of `F` tagged packets, i. e. packets with final packet of frame marker
}

func newDataTrackStats(params dataTrackStatsParams) *dataTrackStats {
	return &dataTrackStats{
		params: params,
	}
}

func (d *dataTrackStats) Update(packet *datatrack.Packet, arrivalTime int64) {
	d.lock.Lock()
	defer d.lock.Unlock()

	if d.endTime != 0 {
		return
	}

	if d.startTime == 0 {
		d.startTime = arrivalTime
		d.highestSequenceNumber = packet.SequenceNumber
		d.numPackets = 1
	} else {
		diff := packet.SequenceNumber - d.highestSequenceNumber
		switch {
		case diff == 0: // duplicate
			return

		case diff > (1 << 15): // out of order
			d.numPackets++
			d.numPacketsOutOfOrder++
			if d.numPacketsLost > 0 {
				d.numPacketsLost--
			}

		default: // in order
			d.numPackets++
			d.numPacketsLost += int(diff) - 1
			d.highestSequenceNumber = packet.SequenceNumber
		}
	}

	if packet.IsFinalOfFrame {
		d.numFrames++
	}
}

func (d *dataTrackStats) Close() {
	d.lock.Lock()
	defer d.lock.Unlock()

	d.endTime = mono.UnixNano()

	duration := time.Duration(d.endTime - d.startTime).Seconds()
	fps := float64(d.numFrames) / duration

	d.params.Logger.Infow(
		"data track stats",
		"duration", duration,
		"numPackets", d.numPackets,
		"numPacketsLost", d.numPacketsLost,
		"numPacketsOutOfOrder", d.numPacketsOutOfOrder,
		"numFrames", d.numFrames,
		"fps", fps,
	)
}
