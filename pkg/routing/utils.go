package routing

import (
	"errors"
	"strings"

	"github.com/livekit/protocol/livekit"
)

func participantKey(roomName livekit.RoomName, identity livekit.ParticipantIdentity) string {
	return string(roomName) + "|" + string(identity)
}

func parseParticipantKey(pkey string) (roomName livekit.RoomName, identity livekit.ParticipantIdentity, err error) {
	parts := strings.Split(pkey, "|")
	if len(parts) != 2 {
		err = errors.New("invalid participant key")
		return
	}

	return livekit.RoomName(parts[0]), livekit.ParticipantIdentity(parts[1]), nil
}
