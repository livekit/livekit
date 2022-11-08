package serverlogger

import (
	"github.com/go-logr/logr"
	"github.com/pion/logging"
	"go.uber.org/zap/zapcore"

	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/config"
)

var (
	pionLevel           zapcore.Level
	pionIgnoredPrefixes = map[string][]string{
		"ice": {
			"pingAllCandidates called with no candidate pairs",
			"failed to send packet",
			"Ignoring remote candidate with tcpType active",
		},
		"pc": {
			"Failed to accept RTCP stream is already closed",
			"Failed to accept RTP stream is already closed",
			"Incoming unhandled RTCP ssrc",
		},
		"tcp_mux": {
			"Error reading first packet from",
			"error closing connection",
		},
	}
)

// implements webrtc.LoggerFactory
type LoggerFactory struct {
	logger logr.Logger
}

func NewLoggerFactory(logger logr.Logger) *LoggerFactory {
	if logger.GetSink() == nil {
		logger = logr.Discard()
	}
	return &LoggerFactory{
		logger: logger,
	}
}

func (f *LoggerFactory) NewLogger(scope string) logging.LeveledLogger {
	return &logAdapter{
		logger:          f.logger.WithName(scope),
		level:           pionLevel,
		ignoredPrefixes: pionIgnoredPrefixes[scope],
	}
}

// Note: only pass in logr.Logger with default depth
func SetLogger(l logr.Logger) {
	logger.SetLogger(l, "livekit")
}

func InitFromConfig(config config.LoggingConfig) {
	pionLevel = logger.ParseZapLevel(config.PionLevel)
	logger.InitFromConfig(config.Config, "livekit")
}
