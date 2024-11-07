package sendsidebwe

import (
	"fmt"

	"github.com/livekit/protocol/logger"
)

// ---------------------------------------------------------------------------

type CongestionState int

const (
	CongestionStateNone CongestionState = iota
	CongestionStateEarlyWarning
	CongestionStateCongested
	CongestionStateCongestionRelieving
)

func (c CongestionState) String() string {
	switch c {
	case CongestionStateNone:
		return "NONE"
	case CongestionStateEarlyWarning:
		return "EARLY_WARNING"
	case CongestionStateCongested:
		return "CONGESTED"
	case CongestionStateCongestionRelieving:
		return "CONGESTiON_RELIEVING"
	default:
		return fmt.Sprintf("%d", int(c))
	}
}

// ---------------------------------------------------------------------------

type SendSideBWE struct {
	*TransportWideSequenceNumber
	*PacketTracker
}

func NewSendSideBWE(logger logger.Logger) *SendSideBWE {
	return &SendSideBWE{
		TransportWideSequenceNumber: NewTransportWideSequenceNumber(),
		PacketTracker:               NewPacketTracker(logger),
	}
}

func (s *SendSideBWE) Stop() {
	s.PacketTracker.Stop()
}

// ------------------------------------------------
