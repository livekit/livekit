package logger

import (
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

var (
	logger *zap.SugaredLogger
)

func getLogger() *zap.SugaredLogger {
	if logger == nil {
		InitDevelopment("")
	}
	return logger
}

func initLogger(config zap.Config, level string) {
	if level != "" {
		lvl := zapcore.Level(0)
		if err := lvl.UnmarshalText([]byte(level)); err == nil {
			config.Level = zap.NewAtomicLevelAt(lvl)
		}
	}
	l, _ := config.Build(zap.AddCallerSkip(1))
	logger = l.Sugar()
}

func InitProduction(logLevel string) {
	initLogger(zap.NewProductionConfig(), logLevel)

}

func InitDevelopment(logLevel string) {
	initLogger(zap.NewDevelopmentConfig(), logLevel)
}

func Debugw(msg string, keysAndValues ...interface{}) {
	getLogger().Debugw(msg, keysAndValues...)
}

func Infow(msg string, keysAndValues ...interface{}) {
	getLogger().Infow(msg, keysAndValues...)
}

func Warnw(msg string, keysAndValues ...interface{}) {
	getLogger().Warnw(msg, keysAndValues...)
}

func Errorw(msg string, keysAndValues ...interface{}) {
	getLogger().Errorw(msg, keysAndValues...)
}

func Desugar() *zap.Logger {
	return getLogger().Desugar()
}
