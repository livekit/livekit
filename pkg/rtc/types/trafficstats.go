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

package types

import (
	"time"

	"github.com/livekit/protocol/livekit"
)

type TrafficStats struct {
	StartTime         time.Time
	EndTime           time.Time
	Packets           uint32
	PacketsLost       uint32
	PacketsPadding    uint32
	PacketsOutOfOrder uint32
	Bytes             uint64
}

type TrafficTypeStats struct {
	TrackType    livekit.TrackType
	StreamType   livekit.StreamType
	TrafficStats *TrafficStats
}

type TrafficLoad struct {
	TrafficTypeStats []*TrafficTypeStats
}

func RTPStatsDiffToTrafficStats(before, after *livekit.RTPStats) *TrafficStats {
	if after == nil {
		return nil
	}

	startTime := after.StartTime
	if before != nil {
		startTime = before.EndTime
	}

	getAfter := func() *TrafficStats {
		return &TrafficStats{
			StartTime:         startTime.AsTime(),
			EndTime:           after.EndTime.AsTime(),
			Packets:           after.Packets,
			PacketsLost:       after.PacketsLost,
			PacketsPadding:    after.PacketsPadding,
			PacketsOutOfOrder: after.PacketsOutOfOrder,
			Bytes:             after.Bytes + after.BytesDuplicate + after.BytesPadding,
		}
	}

	if before == nil {
		return getAfter()
	}

	if (after.Packets - before.Packets) > (1 << 31) {
		// after packets < before packets, probably got reset, just return after
		return getAfter()
	}
	if ((after.Bytes + after.BytesDuplicate + after.BytesPadding) - (before.Bytes + before.BytesDuplicate + before.BytesPadding)) > (1 << 63) {
		// after bytes < before bytes, probably got reset, just return after
		return getAfter()
	}

	packetsLost := uint32(0)
	if after.PacketsLost >= before.PacketsLost {
		packetsLost = after.PacketsLost - before.PacketsLost
	}
	return &TrafficStats{
		StartTime:         startTime.AsTime(),
		EndTime:           after.EndTime.AsTime(),
		Packets:           after.Packets - before.Packets,
		PacketsLost:       packetsLost,
		PacketsPadding:    after.PacketsPadding - before.PacketsPadding,
		PacketsOutOfOrder: after.PacketsOutOfOrder - before.PacketsOutOfOrder,
		Bytes:             (after.Bytes + after.BytesDuplicate + after.BytesPadding) - (before.Bytes + before.BytesDuplicate + before.BytesPadding),
	}
}

func AggregateTrafficStats(statsList ...*TrafficStats) *TrafficStats {
	if len(statsList) == 0 {
		return nil
	}

	startTime := time.Time{}
	endTime := time.Time{}

	packets := uint32(0)
	packetsLost := uint32(0)
	packetsPadding := uint32(0)
	packetsOutOfOrder := uint32(0)
	bytes := uint64(0)

	for _, stats := range statsList {
		if startTime.IsZero() || startTime.After(stats.StartTime) {
			startTime = stats.StartTime
		}

		if endTime.IsZero() || endTime.Before(stats.EndTime) {
			endTime = stats.EndTime
		}

		packets += stats.Packets
		packetsLost += stats.PacketsLost
		packetsPadding += stats.PacketsPadding
		packetsOutOfOrder += stats.PacketsOutOfOrder
		bytes += stats.Bytes
	}

	if endTime.IsZero() {
		endTime = time.Now()
	}
	return &TrafficStats{
		StartTime:         startTime,
		EndTime:           endTime,
		Packets:           packets,
		PacketsLost:       packetsLost,
		PacketsPadding:    packetsPadding,
		PacketsOutOfOrder: packetsOutOfOrder,
		Bytes:             bytes,
	}
}

func TrafficLoadToTrafficRate(trafficLoad *TrafficLoad) (
	packetRateIn float64,
	byteRateIn float64,
	packetRateOut float64,
	byteRateOut float64,
) {
	if trafficLoad == nil {
		return
	}

	for _, trafficTypeStat := range trafficLoad.TrafficTypeStats {
		elapsed := trafficTypeStat.TrafficStats.EndTime.Sub(trafficTypeStat.TrafficStats.StartTime).Seconds()
		packetRate := float64(trafficTypeStat.TrafficStats.Packets) / elapsed
		byteRate := float64(trafficTypeStat.TrafficStats.Bytes) / elapsed
		switch trafficTypeStat.StreamType {
		case livekit.StreamType_UPSTREAM:
			packetRateIn += packetRate
			byteRateIn += byteRate
		case livekit.StreamType_DOWNSTREAM:
			packetRateOut += packetRate
			byteRateOut += byteRate
		}
	}
	return
}
