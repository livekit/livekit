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

type SendSideBWEParams struct {
	Logger logger.Logger
}

type SendSideBWE struct {
	params SendSideBWEParams

	*TransportWideSequenceNumber
	*CongestionDetector
}

func NewSendSideBWE(params SendSideBWEParams) *SendSideBWE {
	return &SendSideBWE{
		params:                      params,
		TransportWideSequenceNumber: NewTransportWideSequenceNumber(),
		CongestionDetector: NewCongestionDetector(CongestionDetectorParams{
			// SSBWE-TODO: need to pass in params from config
			Config: DefaultCongestionDetectorConfig,
			Logger: params.Logger,
		}),
	}
}

func (s *SendSideBWE) Stop() {
	s.CongestionDetector.Stop()
}

// ------------------------------------------------
