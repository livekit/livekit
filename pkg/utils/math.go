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

import "slices"

// Median gets median value for an array
func Median[T float32](input []T) T {
	num := len(input)
	switch num {
	case 0:
		return 0
	case 1:
		return input[0]
	}
	slices.Sort(input)
	if num%2 != 0 {
		return input[num/2]
	}
	left := input[num/2-1]
	right := input[num/2]
	return (left + right) / 2
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
