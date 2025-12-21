/*
 * Copyright 2023 LiveKit, Inc
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package utils

import (
	"github.com/livekit/protocol/logger"
	"go.uber.org/zap/zapcore"
)

type CountedLogger interface {
	Log(msg string, keysAndValues ...any)
	ErrorLog(msg string, err error, keysAndValues ...any)
	SetLogger(lgr logger.Logger)
	Counter() int
}

// -----------------------------------

type CountedLoggerLevel int

const (
	CountedLoggerLevelDebug CountedLoggerLevel = iota
	CountedLoggerLevelInfo
	CountedLoggerLevelWarn
	CountedLoggerLevelError
)

func (c CountedLoggerLevel) String() string {
	switch c {
	case CountedLoggerLevelDebug:
		return "debug"
	case CountedLoggerLevelInfo:
		return "info"
	case CountedLoggerLevelWarn:
		return "warn"
	case CountedLoggerLevelError:
		return "error"
	default:
		return "debug"
	}
}

// -----------------------------------

type countedLoggerBase struct {
	counter int
	lgr     logger.Logger
	level   zapcore.Level
	check   func() bool
}

func newCountedLoggerBase(
	lgr logger.Logger,
	level CountedLoggerLevel,
	check func() bool,
) *countedLoggerBase {
	return &countedLoggerBase{
		lgr:   lgr,
		level: logger.ParseZapLevel(level.String()),
		check: check,
	}
}

func (c *countedLoggerBase) Log(msg string, keysAndValues ...any) {
	c.counter++
	if !c.check() {
		return
	}

	switch c.level {
	case zapcore.DebugLevel:
		c.lgr.Debugw(msg, append(keysAndValues, "counter", c.counter)...)
	case zapcore.InfoLevel:
		c.lgr.Infow(msg, append(keysAndValues, "counter", c.counter)...)
	default:
		c.lgr.Debugw(msg, append(keysAndValues, "counter", c.counter)...)
	}
}

func (c *countedLoggerBase) ErrorLog(msg string, err error, keysAndValues ...any) {
	c.counter++
	if !c.check() {
		return
	}

	switch c.level {
	case zapcore.WarnLevel:
		c.lgr.Warnw(msg, err, append(keysAndValues, "counter", c.counter)...)
	case zapcore.ErrorLevel:
		c.lgr.Errorw(msg, err, append(keysAndValues, "counter", c.counter)...)
	default:
		c.lgr.Warnw(msg, err, append(keysAndValues, "counter", c.counter)...)
	}
}

func (c *countedLoggerBase) SetLogger(lgr logger.Logger) {
	c.lgr = lgr
}

func (c *countedLoggerBase) Counter() int {
	return c.counter
}

// -----------------------------------

type allLogger struct {
	*countedLoggerBase
}

func NewAllLogger(lgr logger.Logger, level CountedLoggerLevel) CountedLogger {
	a := &allLogger{}
	a.countedLoggerBase = newCountedLoggerBase(lgr, level, a.check)
	return a
}

func (a *allLogger) check() bool {
	return true
}

// -----------------------------------

// logs `Initial` number of samples and then samples spaced apart by `Then`
type PeriodicLoggerParams struct {
	Initial int
	Then    int
}

type periodicLogger struct {
	*countedLoggerBase
	params PeriodicLoggerParams
}

func NewPeriodicLogger(lgr logger.Logger, level CountedLoggerLevel, params PeriodicLoggerParams) CountedLogger {
	p := &periodicLogger{
		params: params,
	}
	p.countedLoggerBase = newCountedLoggerBase(lgr, level, p.check)
	return p
}

func (p *periodicLogger) check() bool {
	return p.counter <= p.params.Initial || (p.counter-p.params.Initial)%p.params.Then == 0
}

// -----------------------------------

// logs samples `(counter % Base^n) == 0` for `n = 0, 1, 2, ...`
// for example with `Base = 5`, it will log samples at 1, 2, 3, 4, 5, 10, 15, 20, 25, 50, 75, 100, 125, 250, 375, ...
type ExponentialLoggerParams struct {
	Base int
}

type exponentialLogger struct {
	*countedLoggerBase
	params  ExponentialLoggerParams
	current int
}

func NewExponentialLogger(lgr logger.Logger, level CountedLoggerLevel, params ExponentialLoggerParams) CountedLogger {
	e := &exponentialLogger{
		params:  params,
		current: 1,
	}
	e.countedLoggerBase = newCountedLoggerBase(lgr, level, e.check)
	return e
}

func (e *exponentialLogger) check() bool {
	if e.counter == e.current*e.params.Base {
		e.current *= e.params.Base
	}
	return e.counter%e.current == 0
}

// -----------------------------------
