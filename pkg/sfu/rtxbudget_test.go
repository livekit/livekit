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

package sfu

import (
	"testing"

	"github.com/pion/webrtc/v4"
	"github.com/stretchr/testify/require"
)

func TestRTXBudget(t *testing.T) {
	t.Run("no cap without a ratio", func(t *testing.T) {
		b := newRTXBudget(RTXBudgetLimit{BurstBytes: 100})
		require.Nil(t, b)
		require.True(t, b.allow())
		b.spend(1_000_000)
		b.onMediaSent(10)
		require.True(t, b.allow())
	})

	t.Run("starts with the burst", func(t *testing.T) {
		b := newRTXBudget(RTXBudgetLimit{MaxRatio: 0.3, BurstBytes: 3000})
		require.True(t, b.allow())
		b.spend(2999)
		require.True(t, b.allow())
		b.spend(1)
		require.False(t, b.allow())
	})

	t.Run("burst holds at least one full packet", func(t *testing.T) {
		b := newRTXBudget(RTXBudgetLimit{MaxRatio: 0.3})
		b.spend(minRTXBurstBytes - 1)
		require.True(t, b.allow())
	})

	t.Run("an overdraw is paid back from media sent at the ratio", func(t *testing.T) {
		b := newRTXBudget(RTXBudgetLimit{MaxRatio: 0.5, BurstBytes: 2000})
		b.spend(2900) // overdrawn by 900
		require.False(t, b.allow())

		b.onMediaSent(1800) // pays back exactly 900
		require.False(t, b.allow())
		b.onMediaSent(2)
		require.True(t, b.allow())
	})

	t.Run("refill stops at the burst", func(t *testing.T) {
		b := newRTXBudget(RTXBudgetLimit{MaxRatio: 0.5, BurstBytes: 2000})
		b.onMediaSent(1_000_000)
		require.True(t, b.allow()) // refills, but only up to the burst
		b.spend(2000)
		require.False(t, b.allow())
	})
}

func TestRTXBudgetConfigForKind(t *testing.T) {
	c := RTXBudgetConfig{
		Audio: RTXBudgetLimit{MaxRatio: 0.1},
		Video: RTXBudgetLimit{MaxRatio: 0.3},
	}
	require.Equal(t, 0.1, c.forKind(webrtc.RTPCodecTypeAudio).MaxRatio)
	require.Equal(t, 0.3, c.forKind(webrtc.RTPCodecTypeVideo).MaxRatio)
}
