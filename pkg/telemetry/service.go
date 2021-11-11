package telemetry

import (
	"sync"

	"github.com/gammazero/workerpool"
	"github.com/livekit/protocol/logger"
	livekit "github.com/livekit/protocol/proto"
	"github.com/livekit/protocol/webhook"
	"github.com/pion/rtcp"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
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

func (t *TelemetryService) HandleIncomingRTP(participantID, identity string, diff *buffer.Stats) {
	prometheus.IncrementPackets(prometheus.Incoming, uint64(diff.PacketCount))
	prometheus.IncrementBytes(prometheus.Incoming, diff.TotalByte)

	// TODO: analytics service
	// diff.LastExpected, diff.LastReceived, diff.Jitter, diff.LostRate
}

// TODO: skip unmarshal by getting this from receiver instead?
func (t *TelemetryService) HandleIncomingRTCP(participantID, identity string, bytes []byte) {
	pkts, err := rtcp.Unmarshal(bytes)
	if err != nil {
		logger.Errorw("Interceptor failed to unmarshal rtcp packets", err)
		return
	}

	for _, pkt := range pkts {
		switch pkt.(type) {
		case *rtcp.TransportLayerNack:
			prometheus.IncrementNack(prometheus.Incoming)
		case *rtcp.PictureLossIndication:
			prometheus.IncrementPLI(prometheus.Incoming)
		case *rtcp.FullIntraRequest:
			prometheus.IncrementFIR(prometheus.Incoming)
		}
	}

	// TODO: analytics service
}

func (t *TelemetryService) HandleOutgoingRTP(participantID, identity string, pktLen uint64) {
	prometheus.IncrementPackets(prometheus.Outgoing, 1)
	prometheus.IncrementBytes(prometheus.Outgoing, pktLen)

	// TODO: analytics service
}

func (t *TelemetryService) HandleOutgoingRTCP(participantID, identity string, pkts []rtcp.Packet) {
	for _, pkt := range pkts {
		switch pkt.(type) {
		case *rtcp.TransportLayerNack:
			prometheus.IncrementNack(prometheus.Outgoing)
		case *rtcp.PictureLossIndication:
			prometheus.IncrementPLI(prometheus.Outgoing)
		case *rtcp.FullIntraRequest:
			prometheus.IncrementFIR(prometheus.Outgoing)
		}
	}

	t.sendStats(&livekit.AnalyticsStat{
		Kind:          livekit.StreamType_DOWNSTREAM,
		TimeStamp:     timestamppb.Now(),
		Node:          "",
		Sid:           nil,
		ProjectId:     nil,
		ParticipantId: nil,
		RoomName:      nil,
		Jitter:        nil,
		PacketLost:    nil,
		RrTime:        nil,
		BytesSent:     nil,
		BytesReceived: nil,
		AuthToken:     &t.authToken,
	})
	// TODO: analytics service
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
