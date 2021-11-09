// Copyright 2019 Jorn Friedrich Dreyer
// Modified 2021 Serhii Mikhno
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance  the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package logger defines a default implementation of the github.com/go-logr/logr
// interfaces built on top of zerolog (github.com/rs/zerolog) and is the default
// implementation for ion-sfu released binaries.

// This package separates log level into two different concepts:
// - V-Level - verbosity level, number, that every logger has.
//   A higher value that means more logs will be written.
// - Log-level - usual log level (TRACE|DEBUG|INFO).
// Every log row combines those two values.
// You can set log level to TRACE and see all general traces.
// To see more logs just add -v

package logger

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/go-logr/logr"
	"github.com/go-logr/zerologr"
	"github.com/rs/zerolog"
)

const (
	timeFormat = "2006-01-02 15:04:05.000"
)

// GlobalConfig config contains global options
type GlobalConfig struct {
	V int `mapstructure:"v"`
}

// SetGlobalOptions sets the global options, like level against which all info logs will be
// compared.  If this is greater than or equal to the "V" of the logger, the
// message will be logged. Concurrent-safe.
func SetGlobalOptions(config GlobalConfig) {
	lvl := 1 - config.V
	if v := int(zerolog.TraceLevel); lvl < v {
		lvl = v
	} else if v := int(zerolog.InfoLevel); lvl > v {
		lvl = v
	}
	zerolog.SetGlobalLevel(zerolog.Level(lvl))
}

// SetVLevelByStringGlobal does the same as SetGlobalOptions but
// trying to expose verbosity level as more familiar "word-based" log levels
func SetVLevelByStringGlobal(level string) {
	if v, err := zerolog.ParseLevel(level); err == nil {
		zerolog.SetGlobalLevel(v)
	}
}

// Options that can be passed to NewWithOptions
type Options struct {
	// Name is an optional name of the logger
	Name       string
	TimeFormat string
	Output     io.Writer
	// Logger is an instance of zerolog, if nil a default logger is used
	Logger *zerolog.Logger
}

// New returns a logr.Logger, LogSink is implemented by zerolog.
func New() logr.Logger {
	return NewWithOptions(Options{})
}

// NewWithOptions returns a logr.Logger, LogSink is implemented by zerolog.
func NewWithOptions(opts Options) logr.Logger {
	if opts.TimeFormat != "" {
		zerolog.TimeFieldFormat = opts.TimeFormat
	} else {
		zerolog.TimeFieldFormat = timeFormat
	}

	var out io.Writer
	if opts.Output != nil {
		out = opts.Output
	} else {
		out = getOutputFormat()
	}

	if opts.Logger == nil {
		l := zerolog.New(out).With().Timestamp().Logger()
		opts.Logger = &l
	}

	ls := zerologr.NewLogSink(opts.Logger)
	if zerolog.LevelFieldName == "" {
		// Restore field removed by Zerologr
		zerolog.LevelFieldName = "level"
	}
	l := logr.New(ls)
	if opts.Name != "" {
		l = l.WithName(opts.Name)
	}
	return l
}

func getOutputFormat() zerolog.ConsoleWriter {
	output := zerolog.ConsoleWriter{Out: os.Stdout, NoColor: false}
	output.FormatTimestamp = func(i interface{}) string {
		return "[" + i.(string) + "]"
	}
	output.FormatLevel = func(i interface{}) string {
		return strings.ToUpper(fmt.Sprintf("[%-3s]", i))
	}
	output.FormatMessage = func(i interface{}) string {
		_, file, line, _ := runtime.Caller(10)
		return fmt.Sprintf("[%s:%d] => %s", filepath.Base(file), line, i)
	}
	return output
}
