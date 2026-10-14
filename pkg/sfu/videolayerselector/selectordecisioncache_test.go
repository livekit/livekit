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

package videolayerselector

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSelectorDecisionCacheLargeJump(t *testing.T) {
	s := NewSelectorDecisionCache(256, 80)
	s.AddForwarded(10)
	s.AddForwarded(11)

	var expected []selectorDecision
	require.True(t, s.ExpectDecision(12, func(_ uint64, sd selectorDecision) {
		expected = append(expected, sd)
	}))

	jumpTo := uint64(11 + 65535)
	s.AddDropped(jumpTo)

	// skipped entity that was expected is reported missing
	require.Equal(t, []selectorDecision{selectorDecisionMissing}, expected)

	sd, err := s.GetDecision(jumpTo)
	require.NoError(t, err)
	require.Equal(t, selectorDecisionDropped, sd)

	// the missing range wraps the whole ring, so every other slot reads missing
	for e := jumpTo - s.numEntries + 1; e < jumpTo; e++ {
		sd, err = s.GetDecision(e)
		require.NoError(t, err)
		require.Equal(t, selectorDecisionMissing, sd, "entity %d", e)
	}

	_, err = s.GetDecision(jumpTo - s.numEntries)
	require.Error(t, err)
}

func TestSelectorDecisionCacheSmallJump(t *testing.T) {
	s := NewSelectorDecisionCache(256, 80)
	s.AddForwarded(1000)
	s.AddDropped(1100)

	// within nack window -> unknown, older skipped -> missing
	for e := uint64(1001); e < 1100; e++ {
		sd, err := s.GetDecision(e)
		require.NoError(t, err)
		if e >= 1100-80 {
			require.Equal(t, selectorDecisionUnknown, sd, "entity %d", e)
		} else {
			require.Equal(t, selectorDecisionMissing, sd, "entity %d", e)
		}
	}
}

func TestSelectorDecisionCacheJumpIsBounded(t *testing.T) {
	s := NewSelectorDecisionCache(256, 80)
	entity := uint64(1)
	s.AddForwarded(entity)

	// uncapped, each jump walks ~2x the gap (seconds in total), capped it walks ~2x the ring
	start := time.Now()
	for range 10000 {
		entity += 65535
		s.AddDropped(entity)
	}
	require.Less(t, time.Since(start), time.Second)
}
