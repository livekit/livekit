package rtc

import (
	"fmt"

	"github.com/go-logr/logr"
	"github.com/pion/logging"
)

// implements webrtc.LoggerFactory
type loggerFactory struct {
	logger logr.Logger
}

func (f *loggerFactory) NewLogger(scope string) logging.LeveledLogger {
	return &logAdapter{
		logger: f.logger.WithName(scope),
		// treat info as debug
		verbosity: 1,
	}
}

// implements webrtc.LeveledLogger
type logAdapter struct {
	logger logr.Logger
	// 0 - most verbose
	verbosity int
}

func (l *logAdapter) Trace(msg string) {
	l.Tracef(msg)
}

func (l *logAdapter) Tracef(format string, args ...interface{}) {
	l.logger.V(2 + l.verbosity).Info(fmt.Sprintf(format, args...))
}

func (l *logAdapter) Debug(msg string) {
	l.Debugf(msg)
}

func (l *logAdapter) Debugf(format string, args ...interface{}) {
	l.logger.V(1 + l.verbosity).Info(fmt.Sprintf(format, args...))
}

func (l *logAdapter) Info(msg string) {
	l.Infof(msg)
}

func (l *logAdapter) Infof(format string, args ...interface{}) {
	l.logger.V(l.verbosity).Info(fmt.Sprintf(format, args...))
}

func (l *logAdapter) Warn(msg string) {
	l.Warnf(msg)
}

func (l *logAdapter) Warnf(format string, args ...interface{}) {
	l.logger.Info(fmt.Sprintf(format, args...))
}

func (l *logAdapter) Error(msg string) {
	l.Errorf(msg)
}

func (l *logAdapter) Errorf(format string, args ...interface{}) {
	l.logger.Error(nil, fmt.Sprintf(format, args...))
}
