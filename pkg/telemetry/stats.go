package telemetry

import (
	"github.com/livekit/livekit-server/pkg/telemetry/prometheus"
	"github.com/livekit/protocol/livekit"
)

func (t *telemetryService) TrackStats(streamType livekit.StreamType, participantID livekit.ParticipantID, trackID livekit.TrackID, stat *livekit.AnalyticsStat) {
	t.enqueue(func() {
		direction := prometheus.Incoming
		if streamType == livekit.StreamType_DOWNSTREAM {
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
			if streamType == livekit.StreamType_DOWNSTREAM {
				retransmitPackets += stream.RetransmitPackets
				retransmitBytes += stream.RetransmitBytes
			} else {
				// for upstream, we don't account for these separately for now
				packets += stream.RetransmitPackets
				bytes += stream.RetransmitBytes
			}
		}
		prometheus.IncrementRTCP(direction, nacks, plis, firs)
		prometheus.IncrementPackets(direction, uint64(packets), false)
		prometheus.IncrementBytes(direction, bytes, false)
		if retransmitPackets != 0 {
			prometheus.IncrementPackets(direction, uint64(retransmitPackets), true)
		}
		if retransmitBytes != 0 {
			prometheus.IncrementBytes(direction, retransmitBytes, true)
		}

		if worker, ok := t.getWorker(participantID); ok {
			worker.OnTrackStat(trackID, streamType, stat)
		}
	})
}
