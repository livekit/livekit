// Copyright 2023 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package audio

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const (
	samplesPerBatch    = 25
	defaultActiveLevel = 30
	// requires two noisy samples to count
	defaultPercentile      = 10
	defaultObserveDuration = 500 // ms
)

func TestAudioLevel(t *testing.T) {
	t.Run("initially to return not noisy, within a few samples", func(t *testing.T) {
		clock := time.Now()
		a := createAudioLevel(defaultActiveLevel, defaultPercentile, defaultObserveDuration)

		_, noisy := a.GetLevel(clock.UnixNano())
		require.False(t, noisy)

		observeSamples(a, 28, 5, clock)
		clock = clock.Add(5 * 20 * time.Millisecond)

		_, noisy = a.GetLevel(clock.UnixNano())
		require.False(t, noisy)
	})

	t.Run("not noisy when all samples are below threshold", func(t *testing.T) {
		clock := time.Now()
		a := createAudioLevel(defaultActiveLevel, defaultPercentile, defaultObserveDuration)

		observeSamples(a, 35, 100, clock)
		clock = clock.Add(100 * 20 * time.Millisecond)

		_, noisy := a.GetLevel(clock.UnixNano())
		require.False(t, noisy)
	})

	t.Run("not noisy when less than percentile samples are above threshold", func(t *testing.T) {
		clock := time.Now()
		a := createAudioLevel(defaultActiveLevel, defaultPercentile, defaultObserveDuration)

		observeSamples(a, 35, samplesPerBatch-2, clock)
		clock = clock.Add((samplesPerBatch - 2) * 20 * time.Millisecond)
		observeSamples(a, 25, 1, clock)
		clock = clock.Add(20 * time.Millisecond)
		observeSamples(a, 35, 1, clock)
		clock = clock.Add(20 * time.Millisecond)

		_, noisy := a.GetLevel(clock.UnixNano())
		require.False(t, noisy)
	})

	t.Run("noisy when higher than percentile samples are above threshold", func(t *testing.T) {
		clock := time.Now()
		a := createAudioLevel(defaultActiveLevel, defaultPercentile, defaultObserveDuration)

		observeSamples(a, 35, samplesPerBatch-16, clock)
		clock = clock.Add((samplesPerBatch - 16) * 20 * time.Millisecond)
		observeSamples(a, 25, 8, clock)
		clock = clock.Add(8 * 20 * time.Millisecond)
		observeSamples(a, 29, 8, clock)
		clock = clock.Add(8 * 20 * time.Millisecond)

		level, noisy := a.GetLevel(clock.UnixNano())
		require.True(t, noisy)
		require.Greater(t, level, ConvertAudioLevel(float64(defaultActiveLevel)))
		require.Less(t, level, ConvertAudioLevel(float64(25)))
	})

	t.Run("not noisy when samples are stale", func(t *testing.T) {
		clock := time.Now()
		a := createAudioLevel(defaultActiveLevel, defaultPercentile, defaultObserveDuration)

		observeSamples(a, 25, 100, clock)
		clock = clock.Add(100 * 20 * time.Millisecond)
		level, noisy := a.GetLevel(clock.UnixNano())
		require.True(t, noisy)
		require.Greater(t, level, ConvertAudioLevel(float64(defaultActiveLevel)))
		require.Less(t, level, ConvertAudioLevel(float64(20)))

		// let enough time pass to make the samples stale
		clock = clock.Add(1500 * time.Millisecond)
		level, noisy = a.GetLevel(clock.UnixNano())
		require.Equal(t, float64(0.0), level)
		require.False(t, noisy)
	})
}

func createAudioLevel(activeLevel uint8, minPercentile uint8, observeDuration uint32) *AudioLevel {
	return NewAudioLevel(AudioLevelParams{
		Config: AudioLevelConfig{
			ActiveLevel:    activeLevel,
			MinPercentile:  minPercentile,
			UpdateInterval: observeDuration,
		},
	})
}

func observeSamples(a *AudioLevel, level uint8, count int, baseTime time.Time) {
	for i := 0; i < count; i++ {
		a.Observe(level, 20, baseTime.Add(+time.Duration(i*20)*time.Millisecond).UnixNano())
	}
}
