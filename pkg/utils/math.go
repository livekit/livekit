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

import "sort"

// MedianFloat32 gets median value for an array of float32
func MedianFloat32(input []float32) float32 {
	num := len(input)
	if num == 0 {
		return 0
	} else if num == 1 {
		return input[0]
	}
	sort.Slice(input, func(i, j int) bool {
		return input[i] < input[j]
	})
	if num%2 != 0 {
		return input[num/2]
	}
	left := input[num/2-1]
	right := input[num/2]
	return (left + right) / 2
}
