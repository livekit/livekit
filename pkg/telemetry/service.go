package telemetry

import (
	"github.com/gammazero/workerpool"
	"github.com/livekit/protocol/webhook"
	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
)

type TelemetryService struct {
	notifier webhook.Notifier
	pool     *workerpool.WorkerPool
}

func NewTelemetryService(notifier webhook.Notifier) *TelemetryService {
	return &TelemetryService{
		notifier: notifier,
		pool:     workerpool.New(10),
	}
}

type StatsInterceptorFactory struct {
	t             *TelemetryService
	participantID string
	identity      string
}

func (s *TelemetryService) NewStatsInterceptorFactory(participantID, identity string) *StatsInterceptorFactory {
	return &StatsInterceptorFactory{
		t:             s,
		participantID: participantID,
		identity:      identity,
	}
}

func (f *StatsInterceptorFactory) NewInterceptor(id string) (interceptor.Interceptor, error) {
	return &StatsInterceptor{
		t:             f.t,
		participantID: f.participantID,
		identity:      f.identity,
	}, nil
}

type StatsInterceptor struct {
	interceptor.NoOp

	t             *TelemetryService
	participantID string
	identity      string
}

// --- Incoming ---

// BindRTCPReader lets you modify any incoming RTCP packets. It is called once per sender/receiver, however this might
// change in the future. The returned method will be called once per packet batch.
func (s *StatsInterceptor) BindRTCPReader(reader interceptor.RTCPReader) interceptor.RTCPReader {
	return interceptor.RTCPReaderFunc(func(bytes []byte, attributes interceptor.Attributes) (int, interceptor.Attributes, error) {
		s.t.HandleIncomingRTCP(s.participantID, s.identity, bytes)
		return reader.Read(bytes, attributes)
	})
}

// BindRemoteStream lets you modify any incoming RTP packets. It is called once for per RemoteStream. The returned method
// will be called once per rtp packet.
func (s *StatsInterceptor) BindRemoteStream(_ *interceptor.StreamInfo, reader interceptor.RTPReader) interceptor.RTPReader {
	return interceptor.RTPReaderFunc(func(payload []byte, attributes interceptor.Attributes) (int, interceptor.Attributes, error) {
		s.t.HandleIncomingRTP(s.participantID, s.identity, uint64(len(payload)))
		return reader.Read(payload, attributes)
	})
}

// --- Outgoing ---

// BindRTCPWriter lets you modify any outgoing RTCP packets. It is called once per PeerConnection. The returned method
// will be called once per packet batch.
func (s *StatsInterceptor) BindRTCPWriter(writer interceptor.RTCPWriter) interceptor.RTCPWriter {
	return interceptor.RTCPWriterFunc(func(pkts []rtcp.Packet, attributes interceptor.Attributes) (int, error) {
		s.t.HandleOutgoingRTCP(s.participantID, s.identity, pkts)
		return writer.Write(pkts, attributes)
	})
}

// BindLocalStream lets you modify any outgoing RTP packets. It is called once for per LocalStream. The returned method
// will be called once per rtp packet.
func (s *StatsInterceptor) BindLocalStream(_ *interceptor.StreamInfo, writer interceptor.RTPWriter) interceptor.RTPWriter {
	return interceptor.RTPWriterFunc(func(header *rtp.Header, payload []byte, attributes interceptor.Attributes) (int, error) {
		s.t.HandleOutgoingRTP(s.participantID, s.identity, uint64(len(payload)))
		return writer.Write(header, payload, attributes)
	})
}
