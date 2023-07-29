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

package routing

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/livekit"
)

func TestUtils_ParticipantKey(t *testing.T) {
	// encode/decode empty
	encoded := ParticipantKey("", "")
	roomName, identity, err := parseParticipantKey(encoded)
	require.NoError(t, err)
	require.Equal(t, livekit.RoomName(""), roomName)
	require.Equal(t, livekit.ParticipantIdentity(""), identity)

	// decode invalid
	_, _, err = parseParticipantKey("abcd")
	require.Error(t, err)

	// encode/decode without delimiter
	encoded = ParticipantKey("room1", "identity1")
	roomName, identity, err = parseParticipantKey(encoded)
	require.NoError(t, err)
	require.Equal(t, livekit.RoomName("room1"), roomName)
	require.Equal(t, livekit.ParticipantIdentity("identity1"), identity)

	// encode/decode with delimiter in roomName
	encoded = ParticipantKey("room1|alter_room1", "identity1")
	roomName, identity, err = parseParticipantKey(encoded)
	require.NoError(t, err)
	require.Equal(t, livekit.RoomName("room1|alter_room1"), roomName)
	require.Equal(t, livekit.ParticipantIdentity("identity1"), identity)

	// encode/decode with delimiter in identity
	encoded = ParticipantKey("room1", "identity1|alter-identity1")
	roomName, identity, err = parseParticipantKey(encoded)
	require.NoError(t, err)
	require.Equal(t, livekit.RoomName("room1"), roomName)
	require.Equal(t, livekit.ParticipantIdentity("identity1|alter-identity1"), identity)

	// encode/decode with delimiter in both and multiple delimiters in both
	encoded = ParticipantKey("room1|alter_room1|again_room1", "identity1|alter-identity1|again-identity1")
	roomName, identity, err = parseParticipantKey(encoded)
	require.NoError(t, err)
	require.Equal(t, livekit.RoomName("room1|alter_room1|again_room1"), roomName)
	require.Equal(t, livekit.ParticipantIdentity("identity1|alter-identity1|again-identity1"), identity)
}
