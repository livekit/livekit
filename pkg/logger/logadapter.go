package serverlogger

import (
	"fmt"
	"strings"

	"github.com/go-logr/logr"
	"go.uber.org/zap/zapcore"
)

// implements webrtc.LeveledLogger
type logAdapter struct {
	logger          logr.Logger
	level           zapcore.Level
	ignoredPrefixes []string
}

func (l *logAdapter) Trace(msg string) {
	// ignore trace
}

func (l *logAdapter) Tracef(format string, args ...interface{}) {
	// ignore trace
}

func (l *logAdapter) Debug(msg string) {
	if l.level > zapcore.DebugLevel {
		return
	}
	l.logger.V(1).Info(msg)
}

func (l *logAdapter) Debugf(format string, args ...interface{}) {
	if l.level > zapcore.DebugLevel {
		return
	}
	l.logger.V(1).Info(fmt.Sprintf(format, args...))
}

func (l *logAdapter) Info(msg string) {
	if l.level > zapcore.InfoLevel {
		return
	}
	if l.shouldIgnore(msg) {
		return
	}
	l.logger.Info(msg)
}

func (l *logAdapter) Infof(format string, args ...interface{}) {
	if l.level > zapcore.InfoLevel {
		return
	}
	if l.shouldIgnore(format) {
		return
	}
	l.logger.Info(fmt.Sprintf(format, args...))
}

func (l *logAdapter) Warn(msg string) {
	if l.level > zapcore.WarnLevel {
		return
	}
	if l.shouldIgnore(msg) {
		return
	}
	l.logger.V(-1).Info(msg)
}

func (l *logAdapter) Warnf(format string, args ...interface{}) {
	if l.level > zapcore.WarnLevel {
		return
	}
	if l.shouldIgnore(format) {
		return
	}
	l.logger.V(-1).Info(fmt.Sprintf(format, args...))
}

func (l *logAdapter) Error(msg string) {
	if l.level > zapcore.ErrorLevel {
		return
	}
	if l.shouldIgnore(msg) {
		return
	}
	l.logger.Error(nil, msg)
}

func (l *logAdapter) Errorf(format string, args ...interface{}) {
	if l.level > zapcore.ErrorLevel {
		return
	}
	if l.shouldIgnore(format) {
		return
	}
	l.logger.Error(nil, fmt.Sprintf(format, args...))
}

func (l *logAdapter) shouldIgnore(msg string) bool {
	for _, prefix := range l.ignoredPrefixes {
		if strings.HasPrefix(msg, prefix) {
			return true
		}
	}
	return false
}
