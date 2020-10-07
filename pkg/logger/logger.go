package logger

import (
	"go.uber.org/zap"
)

var logger *zap.SugaredLogger

func GetLogger() *zap.SugaredLogger {
	if logger == nil {
		InitDevelopment()
	}
	return logger
}

func InitProduction() {
	l, _ := zap.NewProduction()
	logger = l.Sugar()
}

func InitDevelopment() {
	l, _ := zap.NewDevelopment()
	logger = l.Sugar()
}
