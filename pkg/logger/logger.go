package logger

import (
	"github.com/go-logr/zapr"
	"github.com/pion/ion-sfu/pkg/buffer"
	"github.com/pion/ion-sfu/pkg/sfu"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

var (
	wrappedLogger *zap.SugaredLogger
	// zap logger
	defaultLogger *zap.Logger
)

func GetLogger() *zap.Logger {
	if defaultLogger == nil {
		InitDevelopment("")
	}
	return defaultLogger
}

// valid levels: debug, info, warn, error, fatal, panic
func InitLogger(config zap.Config, level string, opts ...zap.Option) {
	if level != "" {
		lvl := zapcore.Level(0)
		if err := lvl.UnmarshalText([]byte(level)); err == nil {
			config.Level = zap.NewAtomicLevelAt(lvl)
		}
	}
	// skip one level to remove this helper file
	l, _ := config.Build(append(opts, zap.AddCallerSkip(1))...)
	wrappedLogger = l.Sugar()

	defaultLogger, _ = config.Build(opts...)
	ionLogger := zapr.NewLogger(defaultLogger)
	sfu.Logger = ionLogger
	buffer.Logger = ionLogger
}

func InitProduction(logLevel string) {
	InitLogger(zap.NewProductionConfig(), logLevel)
}

func InitDevelopment(logLevel string) {
	InitLogger(zap.NewDevelopmentConfig(), logLevel)
}

func Debugw(msg string, keysAndValues ...interface{}) {
	if wrappedLogger == nil {
		return
	}
	wrappedLogger.Debugw(msg, keysAndValues...)
}

func Infow(msg string, keysAndValues ...interface{}) {
	if wrappedLogger == nil {
		return
	}
	wrappedLogger.Infow(msg, keysAndValues...)
}

func Warnw(msg string, err error, keysAndValues ...interface{}) {
	if wrappedLogger == nil {
		return
	}
	if err != nil {
		keysAndValues = append([]interface{}{"error", err}, keysAndValues...)
	}
	wrappedLogger.Warnw(msg, keysAndValues...)
}

func Errorw(msg string, err error, keysAndValues ...interface{}) {
	if wrappedLogger == nil {
		return
	}
	if err != nil {
		keysAndValues = append([]interface{}{"error", err}, keysAndValues...)
	}
	wrappedLogger.Errorw(msg, keysAndValues...)
}
