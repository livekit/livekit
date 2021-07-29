package logger

import (
	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	"github.com/pion/ion-sfu/pkg/buffer"
	"github.com/pion/ion-sfu/pkg/sfu"
	"github.com/pion/logging"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

var (
	// pion/ion-sfu
	defaultLogger = logr.Discard()
	// pion/webrtc, pion/turn
	defaultFactory logging.LoggerFactory
)

// Note: already with extra depth 1
func GetLogger() logr.Logger {
	if defaultLogger == logr.Discard() {
		InitDevelopment("")
	}
	return defaultLogger
}

// Note: only pass in logr.Logger with default depth
func SetLogger(l logr.Logger) {
	sfu.Logger = l.WithName("sfu")
	buffer.Logger = sfu.Logger

	defaultLogger = l.WithCallDepth(1).WithName("livekit")
}

func LoggerFactory() logging.LoggerFactory {
	if defaultFactory == nil {
		defaultFactory = logging.NewDefaultLoggerFactory()
	}
	return defaultFactory
}

func SetLoggerFactory(lf logging.LoggerFactory) {
	defaultFactory = lf
}

// valid levels: debug, info, warn, error, fatal, panic
func initLogger(config zap.Config, level string) {
	if level != "" {
		lvl := zapcore.Level(0)
		if err := lvl.UnmarshalText([]byte(level)); err == nil {
			config.Level = zap.NewAtomicLevelAt(lvl)
		}
	}

	logger, _ := config.Build()
	SetLogger(zapr.NewLogger(logger))
}

func InitProduction(logLevel string) {
	initLogger(zap.NewProductionConfig(), logLevel)
}

func InitDevelopment(logLevel string) {
	initLogger(zap.NewDevelopmentConfig(), logLevel)
}

func Debugw(msg string, keysAndValues ...interface{}) {
	defaultLogger.V(1).Info(msg, keysAndValues...)
}

func Infow(msg string, keysAndValues ...interface{}) {
	defaultLogger.Info(msg, keysAndValues...)
}

func Warnw(msg string, err error, keysAndValues ...interface{}) {
	if err != nil {
		keysAndValues = append([]interface{}{"error", err}, keysAndValues...)
	}
	defaultLogger.Info(msg, keysAndValues...)
}

func Errorw(msg string, err error, keysAndValues ...interface{}) {
	defaultLogger.Error(err, msg, keysAndValues...)
}
