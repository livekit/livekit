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

package telemetry

import (
	"github.com/livekit/livekit-server/pkg/telemetry/prometheus"
	"github.com/livekit/protocol/livekit"
)

type StatsKey struct {
	country       string
	streamType    livekit.StreamType
	participantID livekit.ParticipantID
	trackID       livekit.TrackID
	trackSource   livekit.TrackSource
	trackType     livekit.TrackType
	track         bool
}

func StatsKeyForTrack(
	country string,
	streamType livekit.StreamType,
	participantID livekit.ParticipantID,
	trackID livekit.TrackID,
	trackSource livekit.TrackSource,
	trackType livekit.TrackType,
) StatsKey {
	return StatsKey{
		country:       country,
		streamType:    streamType,
		participantID: participantID,
		trackID:       trackID,
		trackSource:   trackSource,
		trackType:     trackType,
		track:         true,
	}
}

func StatsKeyForData(
	country string,
	streamType livekit.StreamType,
	participantID livekit.ParticipantID,
	trackID livekit.TrackID,
) StatsKey {
	return StatsKey{
		country:       country,
		streamType:    streamType,
		participantID: participantID,
		trackID:       trackID,
	}
}

func (t *telemetryService) TrackStats(key StatsKey, stat *livekit.AnalyticsStat) {
	t.enqueue(func() {
		direction := prometheus.Incoming
		if key.streamType == livekit.StreamType_DOWNSTREAM {
			direction = prometheus.Outgoing
		}

		nacks := uint32(0)
		plis := uint32(0)
		firs := uint32(0)
		packets := uint32(0)
		bytes := uint64(0)
		retransmitBytes := uint64(0)
		retransmitPackets := uint32(0)
		for _, stream := range stat.Streams {
			nacks += stream.Nacks
			plis += stream.Plis
			firs += stream.Firs
			packets += stream.PrimaryPackets + stream.PaddingPackets
			bytes += stream.PrimaryBytes + stream.PaddingBytes
			if key.streamType == livekit.StreamType_DOWNSTREAM {
				retransmitPackets += stream.RetransmitPackets
				retransmitBytes += stream.RetransmitBytes
			} else {
				// for upstream, we don't account for these separately for now
				packets += stream.RetransmitPackets
				bytes += stream.RetransmitBytes
			}
			if key.track {
				prometheus.RecordPacketLoss(key.country, direction, key.trackSource, key.trackType, stream.PacketsLost, stream.PrimaryPackets+stream.PaddingPackets)
				prometheus.RecordPacketOutOfOrder(key.country, direction, key.trackSource, key.trackType, stream.PacketsOutOfOrder, stream.PrimaryPackets+stream.PaddingPackets)
				prometheus.RecordRTT(key.country, direction, key.trackSource, key.trackType, stream.Rtt)
				prometheus.RecordJitter(key.country, direction, key.trackSource, key.trackType, stream.Jitter)
			}
		}
		prometheus.IncrementRTCP(key.country, direction, nacks, plis, firs)
		prometheus.IncrementPackets(key.country, direction, uint64(packets), false)
		prometheus.IncrementBytes(key.country, direction, bytes, false)
		if retransmitPackets != 0 {
			prometheus.IncrementPackets(key.country, direction, uint64(retransmitPackets), true)
		}
		if retransmitBytes != 0 {
			prometheus.IncrementBytes(key.country, direction, retransmitBytes, true)
		}

		if worker, ok := t.getWorker(key.participantID); ok {
			worker.OnTrackStat(key.trackID, key.streamType, stat)
		}
	})
}
