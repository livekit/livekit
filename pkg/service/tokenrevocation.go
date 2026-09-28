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

package service

import (
	"errors"

	"github.com/livekit/livekit-server/pkg/config"
)

// getTokenRevocationStore is the wire provider for TokenRevocationStore.
// unlike the other store getters it lives outside wire.go: the config gate
// belongs in exactly one place so every consumer sees the same store - nil
// when the feature is disabled, and a startup error rather than a silent
// no-op when it is enabled on a store that cannot record revocations
func getTokenRevocationStore(s ObjectStore, conf *config.Config) (TokenRevocationStore, error) {
	if !conf.Room.TokenRevocation {
		return nil, nil
	}
	switch store := s.(type) {
	case *RedisStore:
		return store, nil
	case *LocalStore:
		return store, nil
	default:
		return nil, errors.New("room.token_revocation is enabled, but the configured store does not support it")
	}
}
