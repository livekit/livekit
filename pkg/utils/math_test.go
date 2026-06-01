// Copyright 2026 LiveKit, Inc.
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

package utils

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestMedian(t *testing.T) {
	t.Run("Empty slice", func(t *testing.T) {
		assert.Equal(t, float32(0), Median([]float32{}))
		assert.Equal(t, int(0), Median([]int{}))
	})

	t.Run("Single element", func(t *testing.T) {
		assert.Equal(t, float32(42), Median([]float32{42}))
		assert.Equal(t, int(42), Median([]int{42}))
	})

	t.Run("Odd length float32", func(t *testing.T) {
		input := []float32{3.0, 1.0, 2.0}
		assert.Equal(t, float32(2.0), Median(input))
	})

	t.Run("Even length float32 - exact average", func(t *testing.T) {
		input := []float32{1.0, 2.0, 3.0, 4.0}
		assert.Equal(t, float32(2.5), Median(input))
	})

	t.Run("Even length int - integer truncation", func(t *testing.T) {
		input := []int{1, 2}
		// (1 + 2) / 2 = 1.5 -> truncates to 1
		assert.Equal(t, int(1), Median(input))

		inputOddAverage := []int{1, 3}
		// (1 + 3) / 2 = 2
		assert.Equal(t, int(2), Median(inputOddAverage))
	})

	t.Run("Int8 overflow prevention", func(t *testing.T) {
		// Without overflow protection: 120 + 126 = 246 (overflows int8 to -10) -> -10 / 2 = -5
		// With overflow protection: 120 + (126-120)/2 = 123
		input := []int8{120, 126}
		assert.Equal(t, int8(123), Median(input))
	})

	t.Run("Uint8 overflow prevention", func(t *testing.T) {
		input := []uint8{250, 254}
		assert.Equal(t, uint8(252), Median(input))
	})

	t.Run("Immutability test - caller slice is not sorted/mutated", func(t *testing.T) {
		original := []int{3, 1, 4, 2}
		input := slices.Clone(original)

		median := Median(input)
		assert.Equal(t, int(2), median)
		assert.Equal(t, original, input, "Input slice must not be modified by Median")
	})
}

func TestSignum(t *testing.T) {
	t.Run("Integer values", func(t *testing.T) {
		assert.Equal(t, -1, Signum(-42))
		assert.Equal(t, 0, Signum(0))
		assert.Equal(t, 1, Signum(42))
	})

	t.Run("Floating point values", func(t *testing.T) {
		assert.Equal(t, -1, Signum(float32(-0.01)))
		assert.Equal(t, 0, Signum(float32(0.0)))
		assert.Equal(t, 1, Signum(float32(0.01)))
	})
}
