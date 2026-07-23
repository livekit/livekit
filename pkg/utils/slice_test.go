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
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDedupeSlice(t *testing.T) {
	t.Run("Empty slice", func(t *testing.T) {
		var input []int
		result := DedupeSlice(input)
		require.Empty(t, result)
	})

	t.Run("Single element", func(t *testing.T) {
		input := []string{"hello"}
		result := DedupeSlice(input)
		require.Equal(t, []string{"hello"}, result)
	})

	t.Run("Unsorted slice with duplicates", func(t *testing.T) {
		input := []int{4, 2, 4, 1, 3, 2}
		result := DedupeSlice(input)
		require.Equal(t, []int{1, 2, 3, 4}, result)
	})

	t.Run("Already sorted and unique", func(t *testing.T) {
		input := []string{"apple", "banana", "cherry"}
		result := DedupeSlice(input)
		require.Equal(t, []string{"apple", "banana", "cherry"}, result)
	})
}
