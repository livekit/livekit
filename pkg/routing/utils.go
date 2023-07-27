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
	"fmt"
	"strings"

	"github.com/jxskiss/base62"

	"github.com/livekit/protocol/livekit"
)

func ParticipantKeyLegacy(roomName livekit.RoomName, identity livekit.ParticipantIdentity) livekit.ParticipantKey {
	return livekit.ParticipantKey(string(roomName) + "|" + string(identity))
}

func parseParticipantKeyLegacy(pkey livekit.ParticipantKey) (roomName livekit.RoomName, identity livekit.ParticipantIdentity, err error) {
	parts := strings.Split(string(pkey), "|")
	if len(parts) == 2 {
		roomName = livekit.RoomName(parts[0])
		identity = livekit.ParticipantIdentity(parts[1])
		return
	}

	err = fmt.Errorf("invalid participant key: %s", pkey)
	return
}

func ParticipantKey(roomName livekit.RoomName, identity livekit.ParticipantIdentity) livekit.ParticipantKey {
	return livekit.ParticipantKey(encode(string(roomName), string(identity)))
}

func parseParticipantKey(pkey livekit.ParticipantKey) (roomName livekit.RoomName, identity livekit.ParticipantIdentity, err error) {
	parts, err := decode(string(pkey))
	if err != nil {
		return
	}
	if len(parts) == 2 {
		roomName = livekit.RoomName(parts[0])
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
