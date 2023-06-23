package utils

import (
	"fmt"
	"strings"

	"github.com/jxskiss/base62"

	"github.com/livekit/protocol/livekit"
)

func RoomKey(roomName livekit.RoomName, apiKey livekit.ApiKey) livekit.RoomKey {
	return livekit.RoomKey(encode(string(roomName), string(apiKey)))
}

func ParseRoomKey(rkey livekit.RoomKey) (roomName livekit.RoomName, apiKey livekit.ApiKey, err error) {
	parts, err := decode(string(rkey))
	if err != nil {
		return
	}
	if len(parts) == 2 {
		roomName = livekit.RoomName(parts[0])
		apiKey = livekit.ApiKey(parts[1])
		return
	}

	err = fmt.Errorf("invalid room key: %s", rkey)
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
