package telemetry

import (
	"sync"

	"github.com/gammazero/workerpool"
	"github.com/livekit/protocol/logger"
	livekit "github.com/livekit/protocol/proto"
	"github.com/livekit/protocol/webhook"
	"github.com/pion/rtcp"
	"google.golang.org/protobuf/types/known/timestamppb"

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
		notifier:         notifier,
		webhookPool:      workerpool.New(1),
		workers:          make(map[string]*StatsWorker),
		analyticsEnabled: false, // TODO
	}
}

func (t *TelemetryService) NewStatsInterceptorFactory(participantID, identity string) *StatsInterceptorFactory {
	return &StatsInterceptorFactory{
		t:             t,
		participantID: participantID,
		identity:      identity,
	}
}

func (t *TelemetryService) HandleIncomingRTCP(participantID string, bytes []byte) {
	pkts, err := rtcp.Unmarshal(bytes)
	if err != nil {
		logger.Errorw("Interceptor failed to unmarshal rtcp packets", err)
		return
	}

	var nack, pli, fir int32
	for _, pkt := range pkts {
		switch pkt.(type) {
		case *rtcp.TransportLayerNack:
			nack++
		case *rtcp.PictureLossIndication:
			pli++
		case *rtcp.FullIntraRequest:
			fir++
		}
	}

	prometheus.IncrementRTCP(prometheus.Incoming, nack, pli, fir)

	t.RLock()
	if w := t.workers[participantID]; w != nil {
		w.AddRTCP(nack, pli, fir)
	}
	t.RUnlock()
}

func (t *TelemetryService) HandleOutgoingRTP(participantID, identity string, pktLen uint64) {
	prometheus.IncrementPackets(prometheus.Outgoing, 1)
	prometheus.IncrementBytes(prometheus.Outgoing, pktLen)

	// TODO: analytics service
}

func (t *TelemetryService) HandleOutgoingRTCP(participantID, identity string, pkts []rtcp.Packet) {
	var nack, pli, fir int32
	for _, pkt := range pkts {
		switch pkt := pkt.(type) {
		case *rtcp.TransportLayerNack:
			nack++
		case *rtcp.PictureLossIndication:
			pli++
		case *rtcp.FullIntraRequest:
			fir++
		case *rtcp.SenderReport:
			logger.Debugw("outgoing reception report", "report", pkt)
		}
	}

	prometheus.IncrementRTCP(prometheus.Outgoing, nack, pli, fir)

	// TODO: analytics service
}

func (t *TelemetryService) UpdateStats(roomID, participantID string, diff *ParticipantStats) {
	prometheus.IncrementPackets(prometheus.Incoming, uint64(diff.Packets))
	prometheus.IncrementBytes(prometheus.Incoming, diff.Bytes)

	t.sendStats(&livekit.AnalyticsStat{
		AuthToken:     &t.authToken,
		Kind:          livekit.StreamType_UPSTREAM,
		TimeStamp:     timestamppb.Now(),
		Node:          t.nodeID,
		Sid:           &roomID,
		ParticipantId: &participantID,
		Jitter:        &diff.Jitter,
		BytesSent:     &diff.Bytes,
	})
}

func (t *TelemetryService) sendStats(stats *livekit.AnalyticsStat) {
	if t.analyticsEnabled {
		if err := t.stats.Send(&livekit.AnalyticsStats{
			Stats: []*livekit.AnalyticsStat{stats},
		}); err != nil {
			logger.Errorw("failed to send stats", err)
		}
	}
}
