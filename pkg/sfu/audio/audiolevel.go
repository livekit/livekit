package audio

import (
	"math"

	"go.uber.org/atomic"
)

const (
	// duration of audio frames for observe window
	SilentAudioLevel = 127
)

type AudioLevelParams struct {
	ActiveLevel     uint8
	MinPercentile   uint8
	ObserveDuration uint32
}

// keeps track of audio level for a participant
type AudioLevel struct {
	params AudioLevelParams

	currentLevel *atomic.Uint32
	// min duration to be considered active
	minActiveDuration uint32

	// for Observe goroutine use
	// keeps track of current activity
	observeLevel     uint8
	activeDuration   uint32 // ms
	observedDuration uint32 // ms
}

func NewAudioLevel(params AudioLevelParams) *AudioLevel {
	l := &AudioLevel{
		params:            params,
		minActiveDuration: uint32(params.MinPercentile) * params.ObserveDuration / 100,
		currentLevel:      atomic.NewUint32(SilentAudioLevel),
		observeLevel:      SilentAudioLevel,
	}
	return l
}

// Observes a new frame, must be called from the same thread
func (l *AudioLevel) Observe(level uint8, durationMs uint32) {
	l.observedDuration += durationMs

	if level <= l.params.ActiveLevel {
		l.activeDuration += durationMs
		if l.observeLevel > level {
			l.observeLevel = level
		}
	}

	if l.observedDuration >= l.params.ObserveDuration {
		// compute and reset
		if l.activeDuration >= l.minActiveDuration {
			level := uint32(l.observeLevel) - uint32(20*math.Log10(float64(l.activeDuration)/float64(l.params.ObserveDuration)))
			l.currentLevel.Store(level)
		} else {
			l.currentLevel.Store(SilentAudioLevel)
		}
		l.observeLevel = SilentAudioLevel
		l.activeDuration = 0
		l.observedDuration = 0
	}
}

// returns current audio level, 0 (loudest) to 127 (silent)
func (l *AudioLevel) GetLevel() (uint8, bool) {
	level := uint8(l.currentLevel.Load())
	active := level != SilentAudioLevel
	return level, active
}

// convert decibel back to linear
func ConvertAudioLevel(level uint8) float32 {
	const negInv20 = -1.0 / 20
	return float32(math.Pow(10, float64(level)*negInv20))
}
