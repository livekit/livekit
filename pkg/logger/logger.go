package serverlogger

import (
	"github.com/go-logr/logr"
	"github.com/pion/logging"
	"go.uber.org/zap/zapcore"

	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/config"
)

var (
	pionLevel zapcore.Level
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
		logger: f.logger.WithName(scope),
		level:  pionLevel,
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
