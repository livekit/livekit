package routing

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/livekit"
)

func TestUtils_ParticipantKey(t *testing.T) {
	// encode/decode empty
	encoded := participantKey("", "")
	roomName, identity, err := parseParticipantKey(encoded)
	require.NoError(t, err)
	require.Equal(t, livekit.RoomName(""), roomName)
	require.Equal(t, livekit.ParticipantIdentity(""), identity)

	// decode invalid
	_, _, err = parseParticipantKey("abcd")
	require.Error(t, err)

	// encode/decode without delimiter
	encoded = participantKey("room1", "identity1")
	roomName, identity, err = parseParticipantKey(encoded)
	require.NoError(t, err)
	require.Equal(t, livekit.RoomName("room1"), roomName)
	require.Equal(t, livekit.ParticipantIdentity("identity1"), identity)

	// encode/decode with delimiter in roomName
	encoded = participantKey("room1|alter_room1", "identity1")
	roomName, identity, err = parseParticipantKey(encoded)
	require.NoError(t, err)
	require.Equal(t, livekit.RoomName("room1|alter_room1"), roomName)
	require.Equal(t, livekit.ParticipantIdentity("identity1"), identity)

	// encode/decode with delimiter in identity
	encoded = participantKey("room1", "identity1|alter-identity1")
	roomName, identity, err = parseParticipantKey(encoded)
	require.NoError(t, err)
	require.Equal(t, livekit.RoomName("room1"), roomName)
	require.Equal(t, livekit.ParticipantIdentity("identity1|alter-identity1"), identity)

	// encode/decode with delimiter in both and multiple delimiters in both
	encoded = participantKey("room1|alter_room1|again_room1", "identity1|alter-identity1|again-identity1")
	roomName, identity, err = parseParticipantKey(encoded)
	require.NoError(t, err)
	require.Equal(t, livekit.RoomName("room1|alter_room1|again_room1"), roomName)
	require.Equal(t, livekit.ParticipantIdentity("identity1|alter-identity1|again-identity1"), identity)
}
