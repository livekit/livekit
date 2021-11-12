package telemetry

import (
	"sync"

	"github.com/gammazero/workerpool"
	"github.com/livekit/protocol/logger"
	livekit "github.com/livekit/protocol/proto"
	"github.com/livekit/protocol/webhook"
	"github.com/pion/rtcp"

	"github.com/livekit/livekit-server/pkg/telemetry/prometheus"
)

type TelemetryService struct {
	notifier    webhook.Notifier
	webhookPool *workerpool.WorkerPool

	sync.RWMutex
	// one worker per participant
	workers map[string]*StatsWorker

	analyticsEnabled bool
	authToken        string
	nodeID           string
	events           livekit.AnalyticsRecorderService_IngestEventsClient
	stats            livekit.AnalyticsRecorderService_IngestStatsClient
}

func NewTelemetryService(notifier webhook.Notifier) *TelemetryService {
	return &TelemetryService{
		notifier:    notifier,
		webhookPool: workerpool.New(1),
		workers:     make(map[string]*StatsWorker),

		analyticsEnabled: false, // TODO
		authToken:        "",
		nodeID:           "",
	}
}

func (t *TelemetryService) NewStatsInterceptorFactory(participantID, identity string) *StatsInterceptorFactory {
	return &StatsInterceptorFactory{
		t:             t,
		participantID: participantID,
		identity:      identity,
	}
}

func (t *TelemetryService) OnDownstreamPacket(participantID string, bytes int) {
	t.RLock()
	w := t.workers[participantID]
	t.RUnlock()
	if w != nil {
		w.OnDownstreamPacket(bytes)
	}
}

func (t *TelemetryService) HandleRTCP(streamType livekit.StreamType, participantID string, pkts []rtcp.Packet) {
	stats := &ParticipantStats{}
	for _, pkt := range pkts {
		switch pkt := pkt.(type) {
		case *rtcp.TransportLayerNack:
			stats.NackCount++
		case *rtcp.PictureLossIndication:
			stats.PliCount++
		case *rtcp.FullIntraRequest:
			stats.FirCount++
		case *rtcp.ReceiverReport:
			for _, rr := range pkt.Reports {
				if rr.Delay > stats.Delay {
					stats.Delay = rr.Delay
				}
				if rr.Jitter > stats.Jitter {
					stats.Jitter = rr.Jitter
				}
				stats.TotalLost += rr.TotalLost
			}
		}
	}

	direction := prometheus.Incoming
	if streamType == livekit.StreamType_DOWNSTREAM {
		direction = prometheus.Outgoing
	}

	prometheus.IncrementRTCP(direction, stats.NackCount, stats.PliCount, stats.FirCount)

	t.RLock()
	w := t.workers[participantID]
	t.RUnlock()
	if w != nil {
		w.OnRTCP(streamType, stats)
	}
}

func (t *TelemetryService) Report(stats []*livekit.AnalyticsStat) {
	for _, stat := range stats {
		direction := prometheus.Incoming
		if stat.Kind == livekit.StreamType_DOWNSTREAM {
			direction = prometheus.Outgoing
		}

		prometheus.IncrementPackets(direction, stat.TotalPackets)
		prometheus.IncrementBytes(direction, stat.TotalBytes)
	}

	t.sendStats(stats)
}

func (t *TelemetryService) sendStats(stats []*livekit.AnalyticsStat) {
	if t.analyticsEnabled {
		for _, stat := range stats {
			stat.AuthToken = t.authToken
			stat.Node = t.nodeID
		}

		if err := t.stats.Send(&livekit.AnalyticsStats{Stats: stats}); err != nil {
			logger.Errorw("failed to send stats", err)
		}
	}
}
