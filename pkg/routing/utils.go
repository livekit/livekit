package routing

import (
	"fmt"
	"strings"

	"github.com/jxskiss/base62"

	"github.com/livekit/protocol/livekit"
)

func ParticipantKeyLegacy(roomKey livekit.RoomKey, identity livekit.ParticipantIdentity) livekit.ParticipantKey {
	return livekit.ParticipantKey(string(roomKey) + "|" + string(identity))
}

func parseParticipantKeyLegacy(pkey livekit.ParticipantKey) (roomKey livekit.RoomKey, identity livekit.ParticipantIdentity, err error) {
	parts := strings.Split(string(pkey), "|")
	if len(parts) == 2 {
		roomKey = livekit.RoomKey(parts[0])
		identity = livekit.ParticipantIdentity(parts[1])
		return
	}

	err = fmt.Errorf("invalid participant key: %s", pkey)
	return
}

func ParticipantKey(roomKey livekit.RoomKey, identity livekit.ParticipantIdentity) livekit.ParticipantKey {
	return livekit.ParticipantKey(encode(string(roomKey), string(identity)))
}

func parseParticipantKey(pkey livekit.ParticipantKey) (roomKey livekit.RoomKey, identity livekit.ParticipantIdentity, err error) {
	parts, err := decode(string(pkey))
	if err != nil {
		return
	}
	if len(parts) == 2 {
		roomKey = livekit.RoomKey(parts[0])
		identity = livekit.ParticipantIdentity(parts[1])
		return
	}

	err = fmt.Errorf("invalid participant key: %s", pkey)
	return
}

func encode(str ...string) string {
	encoded := make([]string, 0, len(str))
	for _, s := range str {
		encoded = append(encoded, base62.EncodeToString([]byte(s)))
	}
	return strings.Join(encoded, "|")
}

func decode(encoded string) ([]string, error) {
	split := strings.Split(encoded, "|")
	decoded := make([]string, 0, len(split))
	for _, s := range split {
		part, err := base62.DecodeString(s)
		if err != nil {
			return nil, err
		}
		decoded = append(decoded, string(part))
	}
	return decoded, nil
}
