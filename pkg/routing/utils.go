package routing

import (
	"errors"
	"strings"
)

func participantKey(roomName, identity string) string {
	return roomName + "|" + identity
}

func parseParticipantKey(pkey string) (roomName string, identity string, err error) {
	parts := strings.Split(pkey, "|")
	if len(parts) != 2 {
		err = errors.New("invalid participant key")
		return
	}

	return parts[0], parts[1], nil
}
