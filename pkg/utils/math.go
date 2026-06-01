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
// 1. For integer types, if the slice has an even length, the division (/ 2)
//    is performed using integer division, which truncates the result towards zero.
// 2. Uses an overflow-safe formula left + (right-left)/2 to support narrow integer types.
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
