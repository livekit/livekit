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

package rtc

import (
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/utils/guid"
)

func TestPackStreamId(t *testing.T) {
	packed := "PA_123abc|uuid-id"
	pID, trackID := UnpackStreamID(packed)
	require.Equal(t, livekit.ParticipantID("PA_123abc"), pID)
	require.Equal(t, livekit.TrackID("uuid-id"), trackID)

	require.Equal(t, packed, PackStreamID(pID, trackID))
}

func TestPackDataTrackLabel(t *testing.T) {
	pID := livekit.ParticipantID("PA_123abc")
	trackID := livekit.TrackID("TR_b3da25")
	label := "trackLabel"
	packed := "PA_123abc|TR_b3da25|trackLabel"
	require.Equal(t, packed, PackDataTrackLabel(pID, trackID, label))

	p, tr, l := UnpackDataTrackLabel(packed)
	require.Equal(t, pID, p)
	require.Equal(t, trackID, tr)
	require.Equal(t, label, l)
}

func TestChunkProtoBatch(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	var updates []*livekit.ParticipantInfo
	for range 32 {
		updates = append(updates, &livekit.ParticipantInfo{
			Sid:      guid.New(guid.ParticipantPrefix),
			Identity: uuid.NewString(),
			Metadata: strings.Repeat("x", rng.IntN(128*1024)),
		})
	}

	target := 64 * 1024
	batches := ChunkProtoBatch(updates, target)
	var count int
	for _, b := range batches {
		var sum int
		for _, m := range b {
			sum += proto.Size(m)
			count++
		}
		require.True(t, sum < target || len(b) == 1, "batch size exceeds target")
	}
	require.Equal(t, len(updates), count)
}
