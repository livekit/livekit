package utils

import (
	"time"

	"go.uber.org/zap/zapcore"
)

type stopwatchMark struct {
	time  time.Time
	label string
}

type StopwatchSplit struct {
	Label    string
	Duration time.Duration
}

func (s StopwatchSplit) MarshalLogObject(e zapcore.ObjectEncoder) error {
	e.AddString("label", s.Label)
	e.AddDuration("duration", s.Duration)
	return nil
}

type Stopwatch struct {
	marks []*stopwatchMark
}

func NewStopwatch() *Stopwatch {
	return &Stopwatch{
		marks: []*stopwatchMark{
			{
				time:  time.Now(),
				label: "start",
			},
		},
	}
}

func (s *Stopwatch) Mark(label string) {
	s.marks = append(s.marks, &stopwatchMark{
		time:  time.Now(),
		label: label,
	})
}

func (s *Stopwatch) Splits() []StopwatchSplit {
	splits := make([]StopwatchSplit, len(s.marks)-1)
	for i := 1; i < len(s.marks); i++ {
		splits[i-1] = StopwatchSplit{
			Label:    s.marks[i].label,
			Duration: s.marks[i].time.Sub(s.marks[i-1].time),
		}
	}
	return splits
}
