package logger

import (
	"go.uber.org/zap"
)

var (
	logger     *zap.SugaredLogger
	zapOptions = []zap.Option{
		zap.AddCallerSkip(1),
	}
)

func getLogger() *zap.SugaredLogger {
	if logger == nil {
		InitDevelopment()
	}
	return logger
}

func InitProduction() {
	l, _ := zap.NewProduction(zapOptions...)
	logger = l.Sugar()
}

func InitDevelopment() {
	l, _ := zap.NewDevelopment(zapOptions...)
	logger = l.Sugar()
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
