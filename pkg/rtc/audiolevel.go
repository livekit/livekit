package rtc

import (
	"sync/atomic"
)

const (
	// number of samples to compute moving average over
	averageSamples   = 40
	silentAudioLevel = uint8(127)
)

// keeps track of audio level for a participant
type AudioLevel struct {
	levelThreshold uint8
	minPercentile  uint8
	currentLevel   atomic.Value
	// min samples to be considered active
	minSamples uint32

	// for Observe goroutine use
	// keeps track of current running average
	sum           uint32
	activeSamples uint32
	numSamples    uint32
}

func NewAudioLevel(activeLevel uint8, minPercentile uint8) *AudioLevel {
	l := &AudioLevel{
		levelThreshold: activeLevel,
		minPercentile:  minPercentile,
		minSamples:     uint32(minPercentile) * averageSamples / 100,
	}
	l.currentLevel.Store(silentAudioLevel)
	return l
}

// Observes a new sample, must be called from the same thread
func (l *AudioLevel) Observe(level uint8) {
	l.numSamples++

	if level <= l.levelThreshold {
		l.activeSamples++
		l.sum += uint32(level)
	}

	if l.numSamples >= averageSamples {
		// compute and reset
		if l.activeSamples >= l.minSamples {
			l.currentLevel.Store(uint8(l.sum / l.activeSamples))
		} else {
			// nil to represent not noisy
			l.currentLevel.Store(silentAudioLevel)
		}
		l.sum = 0
		l.activeSamples = 0
		l.numSamples = 0
	}
}

// returns current audio level, 0 (loudest) to 127 (silent)
func (l *AudioLevel) GetLevel() (level uint8, noisy bool) {
	level = l.currentLevel.Load().(uint8)
	noisy = level != silentAudioLevel
	return
}

func convertAudioLevel(level uint8) float32 {
	return (127 - float32(level)) / 127
}
