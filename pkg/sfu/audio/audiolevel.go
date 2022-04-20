package audio

import (
	"math"

	"go.uber.org/atomic"
)

const (
	silentAudioLevel = 127
	negInv20         = -1.0 / 20
)

type AudioLevelParams struct {
	ActiveLevel     uint8
	MinPercentile   uint8
	ObserveDuration uint32
	SmoothIntervals uint32
}

// keeps track of audio level for a participant
type AudioLevel struct {
	params AudioLevelParams
	// min duration within an observe duration window to be considered active
	minActiveDuration uint32
	smoothFactor      float64
	activeThreshold   float64

	smoothedLevel atomic.Float64

	loudestObservedLevel uint8
	activeDuration       uint32 // ms
	observedDuration     uint32 // ms
}

func NewAudioLevel(params AudioLevelParams) *AudioLevel {
	l := &AudioLevel{
		params:               params,
		minActiveDuration:    uint32(params.MinPercentile) * params.ObserveDuration / 100,
		smoothFactor:         1,
		activeThreshold:      ConvertAudioLevel(float64(params.ActiveLevel)),
		loudestObservedLevel: silentAudioLevel,
	}

	if l.params.SmoothIntervals > 0 {
		// exponential moving average (EMA), same center of mass with simple moving average (SMA)
		l.smoothFactor = float64(2) / (float64(l.params.SmoothIntervals + 1))
	}

	return l
}

// Observes a new frame, must be called from the same thread
func (l *AudioLevel) Observe(level uint8, durationMs uint32) {
	l.observedDuration += durationMs

	if level <= l.params.ActiveLevel {
		l.activeDuration += durationMs
		if l.loudestObservedLevel > level {
			l.loudestObservedLevel = level
		}
	}

	if l.observedDuration >= l.params.ObserveDuration {
		// compute and reset
		if l.activeDuration >= l.minActiveDuration {
			// adjust loudest observed level by how much of the window was active.
			// Weight will be 0 if active the entire duration
			// > 0 if active for longer than observe duration
			// < 0 if active for less than observe duration
			activityWeight := 20 * math.Log10(float64(l.activeDuration)/float64(l.params.ObserveDuration))
			adjustedLevel := float64(l.loudestObservedLevel) - activityWeight
			linearLevel := ConvertAudioLevel(adjustedLevel)

			// exponential smoothing to dampen transients
			smoothedLevel := l.smoothedLevel.Load()
			smoothedLevel += (linearLevel - smoothedLevel) * l.smoothFactor
			l.smoothedLevel.Store(smoothedLevel)
		} else {
			l.smoothedLevel.Store(0)
		}
		l.loudestObservedLevel = silentAudioLevel
		l.activeDuration = 0
		l.observedDuration = 0
	}
}

// returns current soothed audio level
func (l *AudioLevel) GetLevel() (float64, bool) {
	smoothedLevel := l.smoothedLevel.Load()
	active := smoothedLevel >= l.activeThreshold
	return smoothedLevel, active
}

// convert decibel back to linear
func ConvertAudioLevel(level float64) float64 {
	return math.Pow(10, level*negInv20)
}
