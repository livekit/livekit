package rtc

import (
	"math"
	"sync/atomic"
)

const (
	// duration of audio frames for observe window
	observeDuration  = 500 // ms
	SilentAudioLevel = 127
)

// keeps track of audio level for a participant
type AudioLevel struct {
	levelThreshold uint8
	currentLevel   uint32
	// min duration to be considered active
	minActiveDuration uint32

	// for Observe goroutine use
	// keeps track of current activity
	observeLevel     uint8
	activeDuration   uint32 // ms
	observedDuration uint32 // ms
}

func NewAudioLevel(activeLevel uint8, minPercentile uint8) *AudioLevel {
	l := &AudioLevel{
		levelThreshold:    activeLevel,
		minActiveDuration: uint32(minPercentile) * observeDuration / 100,
		currentLevel:      SilentAudioLevel,
		observeLevel:      SilentAudioLevel,
	}
	return l
}

// Observes a new frame, must be called from the same thread
func (l *AudioLevel) Observe(level uint8, durationMs uint32) {
	l.observedDuration += durationMs

	if level <= l.levelThreshold {
		l.activeDuration += durationMs
		if l.observeLevel > level {
			l.observeLevel = level
		}
	}

	if l.observedDuration >= observeDuration {
		// compute and reset
		if l.activeDuration >= l.minActiveDuration {
			level := uint32(l.observeLevel) - uint32(20*math.Log10(float64(l.activeDuration)/float64(observeDuration)))
			atomic.StoreUint32(&l.currentLevel, level)
		} else {
			atomic.StoreUint32(&l.currentLevel, SilentAudioLevel)
		}
		l.observeLevel = SilentAudioLevel
		l.activeDuration = 0
		l.observedDuration = 0
	}
}

// returns current audio level, 0 (loudest) to 127 (silent)
func (l *AudioLevel) GetLevel() (uint8, bool) {
	level := uint8(atomic.LoadUint32(&l.currentLevel))
	active := level != SilentAudioLevel
	return level, active
}

// convert decibel back to linear
func ConvertAudioLevel(level uint8) float32 {
	const negInv20 = -1.0 / 20
	return float32(math.Pow(10, float64(level)*negInv20))
}
