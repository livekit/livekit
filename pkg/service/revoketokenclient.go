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
	"context"
	"time"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
)

// matches LiveKit Cloud: 70s on top of the TTL of tokens refreshed by the server
const tokenRevocationDefaultTTL = tokenDefaultTTL + 70*time.Second

// NilRevokeTokenClient is used when token revocation is off
type NilRevokeTokenClient struct{}

func (NilRevokeTokenClient) IsRevokeTokenEnabled(context.Context, livekit.RoomName) bool {
	return false
}

func (NilRevokeTokenClient) RevokeToken(context.Context, livekit.RoomName, livekit.ParticipantIdentity, time.Time) error {
	return nil
}

type storeRevokeTokenClient struct {
	store TokenRevocationStore
	ttl   time.Duration
}

func newStoreRevokeTokenClient(store TokenRevocationStore, ttl time.Duration) *storeRevokeTokenClient {
	if ttl <= 0 {
		ttl = tokenRevocationDefaultTTL
	}
	return &storeRevokeTokenClient{store: store, ttl: ttl}
}

func (c *storeRevokeTokenClient) IsRevokeTokenEnabled(context.Context, livekit.RoomName) bool {
	return true
}

func (c *storeRevokeTokenClient) RevokeToken(ctx context.Context, room livekit.RoomName, identity livekit.ParticipantIdentity, notBefore time.Time) error {
	if err := c.store.RevokeRoomParticipant(ctx, room, identity, notBefore, c.ttl); err != nil {
		logger.Warnw("could not revoke participant tokens", err, "room", room, "participant", identity)
		return err
	}
	return nil
}
