package telemetry

import (
	"github.com/livekit/protocol/logger"
	"github.com/pion/rtcp"

	"github.com/livekit/livekit-server/pkg/telemetry/prometheus"
)

func (s *TelemetryService) HandleIncomingRTCP(participantID, identity string, bytes []byte) {
	pkts, err := rtcp.Unmarshal(bytes)
	if err != nil {
		logger.Errorw("Interceptor failed to unmarshal rtcp packets", err)
		return
	}

	s.pool.Submit(func() {
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
	})

	// TODO: analytics service
}

func (s *TelemetryService) HandleIncomingRTP(participantID, identity string, pktLen uint64) {
	s.pool.Submit(func() {
		prometheus.IncrementPackets(prometheus.Incoming, pktLen)
	})

	// TODO: analytics service
}

func (s *TelemetryService) HandleOutgoingRTCP(participantID, identity string, pkts []rtcp.Packet) {
	s.pool.Submit(func() {
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
	})

	// TODO: analytics service
}

func (s *TelemetryService) HandleOutgoingRTP(participantID, identity string, pktLen uint64) {
	s.pool.Submit(func() {
		prometheus.IncrementPackets(prometheus.Outgoing, pktLen)
	})

	// TODO: analytics service
}
