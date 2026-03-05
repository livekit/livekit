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
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/livekit"

	"github.com/livekit/livekit-server/pkg/config"
)

func addExpiredRevocation(s *LocalStore, identity livekit.ParticipantIdentity, room livekit.RoomName) string {
	id := GenRoomParticipantRevocationIdentifier(identity, room)
	s.lock.Lock()
	defer s.lock.Unlock()
	s.tokenRevocationMap[id] = roomParticipantRevocationMapEntry{
		expiresAt:      time.Now().Add(-time.Minute),
		revocationTime: time.Now().Add(-time.Hour),
	}
	return id
}

func hasRevocationEntry(s *LocalStore, id string) bool {
	s.lock.RLock()
	defer s.lock.RUnlock()
	_, ok := s.tokenRevocationMap[id]
	return ok
}

func TestLocalStoreExpiredRevocation(t *testing.T) {
	s := NewLocalStore()
	expired := addExpiredRevocation(s, "expired", "room")

	require.NoError(t, s.RevokeRoomParticipant(context.Background(), &livekit.RoomParticipantIdentity{
		Room:          "room",
		Identity:      "fresh",
		RevokeTokenTs: time.Now().Unix(),
	}, time.Hour))
	fresh := GenRoomParticipantRevocationIdentifier("fresh", "room")

	revoked, _, err := s.IsRoomParticipantRevoked(context.Background(), "expired", "room")
	require.NoError(t, err)
	require.False(t, revoked)

	s.CleanupRevokedTokens()
	require.False(t, hasRevocationEntry(s, expired))
	require.True(t, hasRevocationEntry(s, fresh))
}

// Review: "Expired revocations accumulate in memory".
// Expired entries have to be removed without another lookup of the same identity.
func TestLocalStoreCleansUpExpiredRevocationsPeriodically(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for the periodic cleanup")
	}

	s := NewLocalStore()
	id := addExpiredRevocation(s, "identity", "room")

	require.Eventually(t, func() bool {
		return !hasRevocationEntry(s, id)
	}, tokenRevocationCleanupInterval+15*time.Second, time.Second)
}

// Review: "Removed participants rejoin after eleven minutes".
func TestLocalStoreRevocationTTL(t *testing.T) {
	s := NewLocalStore()
	require.NoError(t, s.RevokeRoomParticipant(context.Background(), &livekit.RoomParticipantIdentity{
		Room:          "room",
		Identity:      "identity",
		RevokeTokenTs: time.Now().Unix(),
	}, 24*time.Hour))

	s.lock.RLock()
	entry, ok := s.tokenRevocationMap[GenRoomParticipantRevocationIdentifier("identity", "room")]
	s.lock.RUnlock()
	require.True(t, ok)
	require.WithinDuration(t, time.Now().Add(24*time.Hour), entry.expiresAt, 5*time.Second)
}

type recordingRevocationStore struct {
	err     error
	revoked []*livekit.RoomParticipantIdentity
	ttls    []time.Duration
}

func (s *recordingRevocationStore) RevokeRoomParticipant(_ context.Context, identity *livekit.RoomParticipantIdentity, ttl time.Duration) error {
	if s.err != nil {
		return s.err
	}
	s.revoked = append(s.revoked, identity)
	s.ttls = append(s.ttls, ttl)
	return nil
}

func (s *recordingRevocationStore) IsRoomParticipantRevoked(context.Context, livekit.ParticipantIdentity, livekit.RoomName) (bool, *time.Time, error) {
	return false, nil, nil
}

func (s *recordingRevocationStore) CleanupRevokedTokens() {}

func TestRoomManagerRevokeParticipantTokens(t *testing.T) {
	newRoomManager := func(t *testing.T, apiConf config.APIConfig, store TokenRevocationStore) *RoomManager {
		conf, err := config.NewConfig("", true, nil, nil)
		require.NoError(t, err)
		conf.API = apiConf
		return &RoomManager{config: conf, tokenRevocationStore: store}
	}
	req := func() *livekit.RoomParticipantIdentity {
		return &livekit.RoomParticipantIdentity{Room: "room", Identity: "identity"}
	}

	// Review: "Same-second tokens bypass removal".
	t.Run("without revoke_token_ts the leeway is added", func(t *testing.T) {
		store := &recordingRevocationStore{}
		rm := newRoomManager(t, config.DefaultAPIConfig(), store)

		before := time.Now().Add(tokenRevocationLeeway).Unix()
		require.NoError(t, rm.revokeParticipantTokens(context.Background(), req()))
		after := time.Now().Add(tokenRevocationLeeway).Unix()

		require.Len(t, store.revoked, 1)
		require.Equal(t, "room", store.revoked[0].Room)
		require.Equal(t, "identity", store.revoked[0].Identity)
		require.GreaterOrEqual(t, store.revoked[0].RevokeTokenTs, before)
		require.LessOrEqual(t, store.revoked[0].RevokeTokenTs, after)
	})

	t.Run("revoke_token_ts is used as given", func(t *testing.T) {
		store := &recordingRevocationStore{}
		rm := newRoomManager(t, config.DefaultAPIConfig(), store)

		r := req()
		r.RevokeTokenTs = 1234
		require.NoError(t, rm.revokeParticipantTokens(context.Background(), r))

		require.Len(t, store.revoked, 1)
		require.Equal(t, int64(1234), store.revoked[0].RevokeTokenTs)
	})

	t.Run("default ttl", func(t *testing.T) {
		store := &recordingRevocationStore{}
		rm := newRoomManager(t, config.DefaultAPIConfig(), store)

		require.NoError(t, rm.revokeParticipantTokens(context.Background(), req()))
		require.Equal(t, tokenDefaultTTL+time.Minute, store.ttls[0])
	})

	// Review: "Removed participants rejoin after eleven minutes".
	t.Run("configured ttl", func(t *testing.T) {
		store := &recordingRevocationStore{}
		rm := newRoomManager(t, config.APIConfig{TokenRevocationTTL: 48 * time.Hour}, store)

		require.NoError(t, rm.revokeParticipantTokens(context.Background(), req()))
		require.Equal(t, 48*time.Hour, store.ttls[0])
	})

	// Review: "Store failures leave removed tokens valid".
	t.Run("store failure is returned", func(t *testing.T) {
		storeErr := errors.New("store unavailable")
		rm := newRoomManager(t, config.DefaultAPIConfig(), &recordingRevocationStore{err: storeErr})

		require.ErrorIs(t, rm.revokeParticipantTokens(context.Background(), req()), storeErr)
	})

	t.Run("without a store", func(t *testing.T) {
		rm := newRoomManager(t, config.DefaultAPIConfig(), nil)
		require.NoError(t, rm.revokeParticipantTokens(context.Background(), req()))
	})
}
