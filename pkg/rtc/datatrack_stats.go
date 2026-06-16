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
	"os"
	"strconv"
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
	reportInterval        time.Duration
	lastReportTime        int64
	highestSequenceNumber uint16
	numPackets            int
	numPacketsLost        int
	numPacketsOutOfOrder  int
	numFrames             int // count of `F` tagged packets, i. e. packets with final packet of frame marker
	numBytes              int
}

func newDataTrackStats(params dataTrackStatsParams) *dataTrackStats {
	reportInterval := time.Duration(0)
	if raw := os.Getenv("LIVEKIT_DATA_TRACK_STATS_INTERVAL_MS"); raw != "" {
		if intervalMs, err := strconv.Atoi(raw); err == nil && intervalMs > 0 {
			reportInterval = time.Duration(intervalMs) * time.Millisecond
		}
	}

	return &dataTrackStats{
		params:         params,
		reportInterval: reportInterval,
	}
}

func (d *dataTrackStats) Update(packet *datatrack.Packet, arrivalTime int64, payloadLength int) {
	d.lock.Lock()
	defer d.lock.Unlock()

	d.numBytes += payloadLength

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

	if d.reportInterval > 0 && arrivalTime-d.lastReportTime >= int64(d.reportInterval) {
		d.lastReportTime = arrivalTime
		d.logLocked("data track stats sample", arrivalTime)
	}
}

func (d *dataTrackStats) Close() {
	d.lock.Lock()
	defer d.lock.Unlock()

	d.endTime = mono.UnixNano()

	if d.startTime != 0 {
		d.logLocked("data track stats", d.endTime)
	}
}

func (d *dataTrackStats) logLocked(message string, now int64) {
	if d.startTime == 0 {
		return
	}

	duration := time.Duration(now - d.startTime).Seconds()
	fps := 0.0
	if duration > 0 {
		fps = float64(d.numFrames) / duration
	}

	d.params.Logger.Infow(
		message,
		"duration", duration,
		"numPackets", d.numPackets,
		"numPacketsLost", d.numPacketsLost,
		"numPacketsOutOfOrder", d.numPacketsOutOfOrder,
		"numFrames", d.numFrames,
		"fps", fps,
		"numBytes", d.numBytes,
	)
}
