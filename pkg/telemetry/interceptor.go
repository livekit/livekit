package telemetry

import (
	"github.com/livekit/protocol/livekit"
	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
)

func (t *telemetryServiceInternal) NewStatsInterceptorFactory(participantID, identity string) *StatsInterceptorFactory {
	return &StatsInterceptorFactory{
		t:             t,
		participantID: participantID,
		identity:      identity,
	}
}

type StatsInterceptorFactory struct {
	t             TelemetryService
	participantID string
	identity      string
}

func (f *StatsInterceptorFactory) NewInterceptor(_ string) (interceptor.Interceptor, error) {
	return &StatsInterceptor{
		t:             f.t,
		participantID: f.participantID,
		identity:      f.identity,
	}, nil
}

type StatsInterceptor struct {
	interceptor.NoOp

	t             TelemetryService
	participantID string
	identity      string
}

// BindRTCPWriter lets you modify any outgoing RTCP packets. It is called once per PeerConnection. The returned method
// will be called once per packet batch.
func (s *StatsInterceptor) BindRTCPWriter(writer interceptor.RTCPWriter) interceptor.RTCPWriter {
	return interceptor.RTCPWriterFunc(func(pkts []rtcp.Packet, attributes interceptor.Attributes) (int, error) {
		s.t.HandleRTCP(livekit.StreamType_UPSTREAM, s.participantID, pkts)
		return writer.Write(pkts, attributes)
	})
}
