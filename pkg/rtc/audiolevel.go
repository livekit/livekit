package rtc

import (
	"math"
	"sync/atomic"
)

const (
	// number of audio frames for observe window
	observeFrames    = 25 // webrtc default opus frame size 20ms, 25*20=500ms matching default UpdateInterval
	silentAudioLevel = 127
)

// keeps track of audio level for a participant
type AudioLevel struct {
	levelThreshold uint8
	currentLevel   uint32
	// min frames to be considered active
	minActiveFrames uint32

	// for Observe goroutine use
	// keeps track of current activity
	observeLevel uint8
	activeFrames uint32
	numFrames    uint32
}

func NewAudioLevel(activeLevel uint8, minPercentile uint8) *AudioLevel {
	l := &AudioLevel{
		levelThreshold:  activeLevel,
		minActiveFrames: uint32(minPercentile) * observeFrames / 100,
		currentLevel:    silentAudioLevel,
		observeLevel:    silentAudioLevel,
	}
	return l
}

// Observes a new frame, must be called from the same thread
func (l *AudioLevel) Observe(level uint8) {
	l.numFrames++

	if level <= l.levelThreshold {
		l.activeFrames++
		if l.observeLevel > level {
			l.observeLevel = level
		}
	}

	if l.numFrames >= observeFrames {
		// compute and reset
		if l.activeFrames >= l.minActiveFrames {
			const invObserveFrames = 1.0 / observeFrames
			level := uint32(l.observeLevel) - uint32(20*math.Log10(float64(l.activeFrames)*invObserveFrames))
			atomic.StoreUint32(&l.currentLevel, level)
		} else {
			atomic.StoreUint32(&l.currentLevel, silentAudioLevel)
		}
		l.observeLevel = silentAudioLevel
		l.activeFrames = 0
		l.numFrames = 0
	}
}

// returns current audio level, 0 (loudest) to 127 (silent)
func (l *AudioLevel) GetLevel() (uint8, bool) {
	level := uint8(atomic.LoadUint32(&l.currentLevel))
	active := level < l.levelThreshold
	return level, active
}

// convert decibel back to linear
func ConvertAudioLevel(level uint8) float32 {
	const negInv20 = -1.0 / 20
	return float32(math.Pow(10, float64(level)*negInv20))
}
