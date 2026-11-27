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

package types

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/livekit"
)

func TestRTPStatsDiffToTrafficStatsCountsFEC(t *testing.T) {
	before := &livekit.RTPStats{
		Packets:        100,
		Bytes:          100_000,
		BytesDuplicate: 1_000,
		BytesPadding:   500,
		FecPackets:     10,
		FecBytes:       8_000,
	}
	after := &livekit.RTPStats{
		Packets:        200,
		Bytes:          200_000,
		BytesDuplicate: 2_000,
		BytesPadding:   1_000,
		FecPackets:     25,
		FecBytes:       20_000,
	}

	first := RTPStatsDiffToTrafficStats(nil, before)
	require.EqualValues(t, 110, first.Packets)
	require.EqualValues(t, 109_500, first.Bytes)

	diff := RTPStatsDiffToTrafficStats(before, after)
	require.EqualValues(t, 115, diff.Packets, "media and repair packets")
	require.EqualValues(t, 113_500, diff.Bytes, "media, retransmission, padding and repair bytes")
}
