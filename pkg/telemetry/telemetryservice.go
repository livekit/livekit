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

type TelemetryService interface {
	// stats
	NewStatsInterceptorFactory(participantID, identity string) *StatsInterceptorFactory
	AddUpTrack(participantID string, buff *buffer.Buffer)
	OnDownstreamPacket(participantID string, bytes int)
	HandleRTCP(streamType livekit.StreamType, participantID string, pkts []rtcp.Packet)
	Report(ctx context.Context, stats []*livekit.AnalyticsStat)
	UpdateRating(participantID string, trackSid string, kind livekit.StreamType, rating float64)

	// events
	RoomStarted(ctx context.Context, room *livekit.Room)
	RoomEnded(ctx context.Context, room *livekit.Room)
	ParticipantJoined(ctx context.Context, room *livekit.Room, participant *livekit.ParticipantInfo, clientInfo *livekit.ClientInfo)
	ParticipantLeft(ctx context.Context, room *livekit.Room, participant *livekit.ParticipantInfo)
	TrackPublished(ctx context.Context, participantID string, track *livekit.TrackInfo)
	TrackUnpublished(ctx context.Context, participantID string, track *livekit.TrackInfo, ssrc uint32)
	TrackSubscribed(ctx context.Context, participantID string, track *livekit.TrackInfo)
	TrackUnsubscribed(ctx context.Context, participantID string, track *livekit.TrackInfo)
	RecordingStarted(ctx context.Context, ri *livekit.RecordingInfo)
	RecordingEnded(ctx context.Context, ri *livekit.RecordingInfo)
}

type telemetryService struct {
	notifier    webhook.Notifier
	webhookPool *workerpool.WorkerPool

	sync.RWMutex
	// one worker per participant
	workers map[string]*StatsWorker

	analytics AnalyticsService
}

func (t *telemetryService) UpdateRating(participantID string, trackSid string, kind livekit.StreamType, rating float64) {

	t.RLock()
	w := t.workers[participantID]
	t.RUnlock()
	w.UpdateConnectionScores(trackSid, kind, rating)
}

func NewTelemetryService(notifier webhook.Notifier, analytics AnalyticsService) TelemetryService {
	return &telemetryService{
		notifier:    notifier,
		webhookPool: workerpool.New(1),
		workers:     make(map[string]*StatsWorker),
		analytics:   analytics,
	}
}

func (t *telemetryService) AddUpTrack(participantID string, buff *buffer.Buffer) {
	t.RLock()
	w := t.workers[participantID]
	t.RUnlock()
	if w != nil {
		w.AddBuffer(buff)
	}
}

func (t *telemetryService) OnDownstreamPacket(participantID string, bytes int) {
	t.RLock()
	w := t.workers[participantID]
	t.RUnlock()
	if w != nil {
		w.OnDownstreamPacket(bytes)
	}
}

func (t *telemetryService) HandleRTCP(streamType livekit.StreamType, participantID string, pkts []rtcp.Packet) {
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

func (t *telemetryService) Report(ctx context.Context, stats []*livekit.AnalyticsStat) {
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
