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

package flexfec

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLossAggregatorWaitsForEnoughPackets(t *testing.T) {
	var a LossAggregator
	_, ok := a.Update(1000, 0)
	require.False(t, ok, "the first report only sets the baseline")

	// Frequent reports of 4 packets with 1 lost each read as 25% loss on
	// their own; aggregated, every fourth packet over 20 packets is 25% too,
	// but a single loss in 20 packets is 5%, not 25%.
	_, ok = a.Update(1004, 1)
	require.False(t, ok)
	for seq := uint32(1008); seq < 1020; seq += 4 {
		_, ok = a.Update(seq, 1)
		require.False(t, ok)
	}
	fraction, ok := a.Update(1020, 1)
	require.True(t, ok)
	require.EqualValues(t, 1<<8/20, fraction)

	// The next sample starts from scratch.
	_, ok = a.Update(1030, 3)
	require.False(t, ok)
	fraction, ok = a.Update(1040, 5)
	require.True(t, ok)
	require.EqualValues(t, 4<<8/20, fraction)
}

func TestLossAggregatorHandlesCounterEdgeCases(t *testing.T) {
	var a LossAggregator
	a.Update(0xffff_fff0, 0xff_fffe) // cumulative loss -2 as signed 24-bit

	// Sequence wrap and signed 24-bit cumulative loss wrap.
	fraction, ok := a.Update(0x0000_0004, 0x00_0003)
	require.True(t, ok)
	require.EqualValues(t, 5<<8/20, fraction)

	// Duplicates decrease the cumulative loss; it never goes below zero.
	fraction, ok = a.Update(0x0000_0018, 0x00_0001)
	require.True(t, ok)
	require.EqualValues(t, 0, fraction)

	// Loss above the expected packets is capped.
	fraction, ok = a.Update(0x0000_002c, 0x00_0040)
	require.True(t, ok)
	require.EqualValues(t, 255, fraction)

	// A large jump or a step back resets the baseline.
	_, ok = a.Update(0x0001_0000, 0x00_0040)
	require.False(t, ok)
	_, ok = a.Update(0x0000_ff00, 0x00_0040)
	require.False(t, ok)
	fraction, ok = a.Update(0x0000_ff14, 0x00_0042)
	require.True(t, ok)
	require.EqualValues(t, 2<<8/20, fraction)
}
