package rtc_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/livekit/livekit-server/pkg/rtc"
)

const (
	defaultActiveLevel = 30
	// requires two noisy samples to count
	defaultPercentile = 5
)

func TestAudioLevel(t *testing.T) {
	t.Run("initially to return not noisy, within a few samples", func(t *testing.T) {
		a := rtc.NewAudioLevel(defaultActiveLevel, defaultPercentile)
		_, noisy := a.GetLevel()
		assert.False(t, noisy)

		observeSamples(a, 35, 5)
		_, noisy = a.GetLevel()
		assert.False(t, noisy)
	})

	t.Run("not noisy when all samples are below threshold", func(t *testing.T) {
		a := rtc.NewAudioLevel(defaultActiveLevel, defaultPercentile)

		observeSamples(a, 35, 100)
		_, noisy := a.GetLevel()
		assert.False(t, noisy)
	})

	t.Run("not noisy when less than percentile samples are above threshold", func(t *testing.T) {
		a := rtc.NewAudioLevel(defaultActiveLevel, defaultPercentile)

		observeSamples(a, 35, 39)
		observeSamples(a, 25, 1)
		observeSamples(a, 35, 1)

		_, noisy := a.GetLevel()
		assert.False(t, noisy)
	})

	t.Run("noisy when higher than percentile samples are above threshold", func(t *testing.T) {
		a := rtc.NewAudioLevel(defaultActiveLevel, defaultPercentile)

		observeSamples(a, 35, 37)
		observeSamples(a, 25, 1)
		observeSamples(a, 29, 2)

		level, noisy := a.GetLevel()
		assert.True(t, noisy)
		assert.Less(t, level, uint8(defaultActiveLevel))
		assert.Greater(t, level, uint8(25))
	})
}

func observeSamples(a *rtc.AudioLevel, level uint8, count int) {
	for i := 0; i < count; i++ {
		a.Observe(level)
	}
}
