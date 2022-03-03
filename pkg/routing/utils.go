package routing

import (
	"errors"
	"strings"

	"github.com/livekit/protocol/livekit"
)

func participantKey(roomName livekit.RoomName, identity livekit.ParticipantIdentity) livekit.ParticipantKey {
	return livekit.ParticipantKey(string(roomName) + "|" + string(identity))
}

func parseParticipantKey(pkey livekit.ParticipantKey) (roomName livekit.RoomName, identity livekit.ParticipantIdentity, err error) {
	parts := strings.Split(string(pkey), "|")
	if len(parts) != 2 {
		err = errors.New("invalid participant key")
		return
	}

	return livekit.RoomName(parts[0]), livekit.ParticipantIdentity(parts[1]), nil
}
