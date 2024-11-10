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
	CongestionStateEarlyWarningHangover
	CongestionStateCongested
	CongestionStateCongestedHangover
)

func (c CongestionState) String() string {
	switch c {
	case CongestionStateNone:
		return "NONE"
	case CongestionStateEarlyWarning:
		return "EARLY_WARNING"
	case CongestionStateEarlyWarningHangover:
		return "EARLY_WARNING_HANGOVER"
	case CongestionStateCongested:
		return "CONGESTED"
	case CongestionStateCongestedHangover:
		return "CONGESTED_HANGOVER"
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

	*congestionDetector
}

func NewSendSideBWE(params SendSideBWEParams) *SendSideBWE {
	return &SendSideBWE{
		params: params,
		congestionDetector: NewCongestionDetector(congestionDetectorParams{
			Config: params.Config.CongestionDetector,
			Logger: params.Logger,
		}),
	}
}

func (s *SendSideBWE) Stop() {
	s.congestionDetector.Stop()
}

// ------------------------------------------------
