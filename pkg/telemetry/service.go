package telemetry

import (
	"sync"

	"github.com/gammazero/workerpool"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/webhook"
	"github.com/pion/rtcp"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/telemetry/prometheus"
)

type TelemetryService struct {
	notifier    webhook.Notifier
	webhookPool *workerpool.WorkerPool

	sync.RWMutex
	// one worker per participant
	workers map[string]*StatsWorker
}

func NewTelemetryService(notifier webhook.Notifier) *TelemetryService {
	return &TelemetryService{
		notifier:    notifier,
		webhookPool: workerpool.New(1),
		workers:     make(map[string]*StatsWorker),
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

	// TODO: analytics service
}
