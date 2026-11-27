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

package utils

import (
	"cmp"
	"math"
	"slices"
)

// OrderedNumber defines a constraint for numeric types that can be ordered and divided.
type OrderedNumber interface {
	cmp.Ordered
	~int | ~int8 | ~int16 | ~int32 | ~int64 |
		~uint | ~uint8 | ~uint16 | ~uint32 | ~uint64 |
		~uintptr | ~float32 | ~float64
}

// Median gets the median value for a slice without modifying the original slice.
//
// Note:
//  1. For integer types, if the slice has an even length, the division (/ 2)
//     is performed using integer division, which truncates the result towards zero.
//  2. Averages the middle values without overflowing their sum or difference.
func Median[T OrderedNumber](input []T) T {
	num := len(input)
	switch num {
	case 0:
		var zero T
		return zero
	case 1:
		return input[0]
	}

	// Clone the slice to avoid mutating the caller's slice
	sortedInput := slices.Clone(input)
	slices.Sort(sortedInput)

	if num%2 != 0 {
		return sortedInput[num/2]
	}
	left := sortedInput[num/2-1]
	right := sortedInput[num/2]
	if left == right {
		return left
	}
	if T(1)/T(2) != 0 {
		// Sum before dividing to preserve subnormal values. Halve large
		// operands first to keep their sum from overflowing.
		a, b := float64(left), float64(right)
		if math.Abs(a) > math.MaxFloat64/2 || math.Abs(b) > math.MaxFloat64/2 {
			return T(a/2 + b/2)
		}
		return T((a + b) / 2)
	}
	if left < 0 {
		if right >= 0 {
			// Opposite signs make the sum safe, even at the numeric limits.
			return (left + right) / T(2)
		}
		// Start at the upper value so negative integers truncate toward zero.
		return right - (right-left)/T(2)
	}
	return left + (right-left)/T(2)
}

func Signum[T int | int8 | int16 | int32 | int64 | float32 | float64](val T) int {
	switch {
	case val < 0:
		return -1

	case val > 0:
		return 1

	default:
		return 0
	}
}
