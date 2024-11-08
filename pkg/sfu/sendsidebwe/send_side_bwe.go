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
	CongestionStateEarlyWarningRelieving
	CongestionStateCongested
	CongestionStateCongestedRelieving
)

func (c CongestionState) String() string {
	switch c {
	case CongestionStateNone:
		return "NONE"
	case CongestionStateEarlyWarning:
		return "EARLY_WARNING"
	case CongestionStateEarlyWarningRelieving:
		return "EARLY_WARNING_RELIEVING"
	case CongestionStateCongested:
		return "CONGESTED"
	case CongestionStateCongestedRelieving:
		return "CONGESTED_RELIEVING"
	default:
		return fmt.Sprintf("%d", int(c))
	}
}

// ---------------------------------------------------------------------------

type SendSideBWEConfig struct {
	CongestionDetector CongestionDetectorConfig `yaml:"congestion_detector,omitempty"`
}

var (
	DefaultSendSideBWEConfig = SendSideBWEConfig{
		CongestionDetector: DefaultCongestionDetectorConfig,
	}
)

// ---------------------------------------------------------------------------

type SendSideBWEParams struct {
	Config SendSideBWEConfig
	Logger logger.Logger
}

type SendSideBWE struct {
	params SendSideBWEParams

	*CongestionDetector
}

func NewSendSideBWE(params SendSideBWEParams) *SendSideBWE {
	return &SendSideBWE{
		params: params,
		CongestionDetector: NewCongestionDetector(CongestionDetectorParams{
			Config: params.Config.CongestionDetector,
			Logger: params.Logger,
		}),
	}
}

func (s *SendSideBWE) Stop() {
	s.CongestionDetector.Stop()
}

// ------------------------------------------------
