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
	if logger == nil {
		return
	}
	logger.Debugw(msg, keysAndValues...)
}

func Infow(msg string, keysAndValues ...interface{}) {
	if logger == nil {
		return
	}
	logger.Infow(msg, keysAndValues...)
}

func Warnw(msg string, err error, keysAndValues ...interface{}) {
	if logger == nil {
		return
	}
	if err != nil {
		keysAndValues = append([]interface{}{"error", err}, keysAndValues...)
	}
	logger.Warnw(msg, keysAndValues...)
}

func Errorw(msg string, err error, keysAndValues ...interface{}) {
	if logger == nil {
		return
	}
	if err != nil {
		keysAndValues = append([]interface{}{"error", err}, keysAndValues...)
	}
	logger.Errorw(msg, keysAndValues...)
}

func Desugar() *zap.Logger {
	if logger == nil {
		getLogger()
	}
	return logger.Desugar()
}
