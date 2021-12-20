package telemetry

import (
	"context"
	"sync"

	"github.com/gammazero/workerpool"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/webhook"
	"github.com/pion/rtcp"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/telemetry/prometheus"
)

type TelemetryServiceInternal interface {
	TelemetryService
	SendAnalytics()
}

type TelemetryReporter interface {
	Report(ctx context.Context, stats []*livekit.AnalyticsStat)
}

type telemetryServiceInternal struct {
	notifier    webhook.Notifier
	webhookPool *workerpool.WorkerPool

	sync.RWMutex
	// one worker per participant
	workers map[string]*StatsWorker

	analytics AnalyticsService
}

func NewTelemetryServiceInternal(notifier webhook.Notifier, analytics AnalyticsService) TelemetryServiceInternal {
	return &telemetryServiceInternal{
		notifier:    notifier,
		webhookPool: workerpool.New(1),
		workers:     make(map[string]*StatsWorker),
		analytics:   analytics,
	}
}

func (t *telemetryServiceInternal) AddUpTrack(participantID string, buff *buffer.Buffer) {
	t.RLock()
	w := t.workers[participantID]
	t.RUnlock()
	if w != nil {
		w.AddBuffer(buff)
	}
}

func (t *telemetryServiceInternal) OnDownstreamPacket(participantID string, bytes int) {
	t.RLock()
	w := t.workers[participantID]
	t.RUnlock()
	if w != nil {
		w.OnDownstreamPacket(bytes)
	}
}

func (t *telemetryServiceInternal) HandleRTCP(streamType livekit.StreamType, participantID string, pkts []rtcp.Packet) {
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

func (t *telemetryServiceInternal) Report(ctx context.Context, stats []*livekit.AnalyticsStat) {
	for _, stat := range stats {
		direction := prometheus.Incoming
		if stat.Kind == livekit.StreamType_DOWNSTREAM {
			direction = prometheus.Outgoing
		}

		prometheus.IncrementPackets(direction, stat.TotalPackets)
		prometheus.IncrementBytes(direction, stat.TotalBytes)
	}

	t.analytics.SendStats(ctx, stats)
}

func (t *telemetryServiceInternal) SendAnalytics() {
	for _, worker := range t.workers {
		worker.Update()
	}
}
