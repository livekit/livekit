package serverlogger

import (
	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	"github.com/pion/logging"
	"go.uber.org/zap"
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
	lvl := parseLevel(config.Level)
	pionLevel = parseLevel(config.PionLevel)
	if lvl > pionLevel {
		pionLevel = lvl
	}
	zapConfig := zap.Config{
		Level:            zap.NewAtomicLevelAt(lvl),
		Development:      false,
		Encoding:         "console",
		EncoderConfig:    zap.NewDevelopmentEncoderConfig(),
		OutputPaths:      []string{"stderr"},
		ErrorOutputPaths: []string{"stderr"},
	}
	if config.Sample {
		zapConfig.Sampling = &zap.SamplingConfig{
			Initial:    100,
			Thereafter: 100,
		}
	}
	if config.JSON {
		zapConfig.Encoding = "json"
		zapConfig.EncoderConfig = zap.NewProductionEncoderConfig()
	}
	initLogger(zapConfig)
}

// valid levels: debug, info, warn, error, fatal, panic
func initLogger(config zap.Config) {
	l, _ := config.Build()
	zapLogger := zapr.NewLogger(l)
	SetLogger(zapLogger)
}

func parseLevel(level string) zapcore.Level {
	lvl := zapcore.InfoLevel
	if level != "" {
		_ = lvl.UnmarshalText([]byte(level))
	}
	return lvl
}
