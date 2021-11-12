package telemetry

import (
	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
)

type StatsInterceptorFactory struct {
	t             *TelemetryService
	participantID string
	identity      string
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

// BindRTCPReader lets you modify any incoming RTCP packets. It is called once per sender/receiver, however this might
// change in the future. The returned method will be called once per packet batch.
func (s *StatsInterceptor) BindRTCPReader(reader interceptor.RTCPReader) interceptor.RTCPReader {
	return interceptor.RTCPReaderFunc(func(bytes []byte, attributes interceptor.Attributes) (int, interceptor.Attributes, error) {
		s.t.HandleIncomingRTCP(s.participantID, s.identity, bytes)
		return reader.Read(bytes, attributes)
	})
}

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
