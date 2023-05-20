package sendsidebwe

import (
	"github.com/livekit/protocol/logger"
	"github.com/pion/rtcp"
)

type SendSideBWE struct {
	*TransportWideSequenceNumber
	*PacketTracker
	*TWCCFeedback
}

func NewSendSideBWE(logger logger.Logger) *SendSideBWE {
	return &SendSideBWE{
		TransportWideSequenceNumber: NewTransportWideSequenceNumber(),
		PacketTracker:               NewPacketTracker(logger),
		TWCCFeedback:                NewTWCCFeedback(logger),
	}
}

func (s *SendSideBWE) HandleRTCP(report *rtcp.TransportLayerCC) {
	baseSN, arrivals, err := s.TWCCFeedback.HandleRTCP(report)
	if err != nil {
		return
	}

	s.PacketTracker.ProcessFeedback(baseSN, arrivals)
}

// ------------------------------------------------
