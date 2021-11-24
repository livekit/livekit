package telemetry

import (
	"context"
	"sync"

	"google.golang.org/grpc"

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
	analyticsKey     string
	nodeID           string
	events           livekit.AnalyticsRecorderService_IngestEventsClient
	stats            livekit.AnalyticsRecorderService_IngestStatsClient
}

func NewTelemetryService(notifier webhook.Notifier, conn *grpc.ClientConn) *TelemetryService {

	service := &TelemetryService{
		notifier:    notifier,
		webhookPool: workerpool.New(1),
		workers:     make(map[string]*StatsWorker),

		analyticsEnabled: false, // TODO
		analyticsKey:     "",
		nodeID:           "",
	}

	// Setup analytics clients
	if conn != nil {
		client := livekit.NewAnalyticsRecorderServiceClient(conn)
		if client != nil {
			stats, err := client.IngestStats(context.Background())
			if err != nil {
				logger.Errorw("Failed to create analytics stats client", err)
				return service
			}
			events, err := client.IngestEvents(context.Background())
			if err != nil {
				logger.Errorw("Failed to create analytics events client", err)
				return service
			}
			service.stats = stats
			service.events = events
			service.analyticsEnabled = true
		}
	}
	return  service
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
	stats := &livekit.AnalyticsStat{}
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
				if delay := uint64(rr.Delay); delay > stats.Delay {
					stats.Delay = delay
				}
				if jitter := float64(rr.Jitter); jitter > stats.Jitter {
					stats.Jitter = jitter
				}
				stats.PacketLost += uint64(rr.TotalLost)
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
			stat.AnalyticsKey = t.analyticsKey
			stat.Node = t.nodeID
		}

		if err := t.stats.Send(&livekit.AnalyticsStats{Stats: stats}); err != nil {
			logger.Errorw("failed to send stats", err)
		}
	}
}
