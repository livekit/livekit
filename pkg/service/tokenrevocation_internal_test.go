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

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/protocol/rpc/rpcfakes"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing/routingfakes"
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

	require.NoError(t, s.RevokeRoomParticipant(context.Background(), "room", "fresh", time.Now(), time.Hour))
	fresh := GenRoomParticipantRevocationIdentifier("fresh", "room")

	revoked, _, err := s.IsRoomParticipantRevoked(context.Background(), "expired", "room")
	require.NoError(t, err)
	require.False(t, revoked)

	s.CleanupRevokedTokens()
	require.False(t, hasRevocationEntry(s, expired))
	require.True(t, hasRevocationEntry(s, fresh))
}

func TestLocalStoreRevocationTTL(t *testing.T) {
	s := NewLocalStore()
	require.NoError(t, s.RevokeRoomParticipant(context.Background(), "room", "identity", time.Now(), 24*time.Hour))

	s.lock.RLock()
	entry, ok := s.tokenRevocationMap[GenRoomParticipantRevocationIdentifier("identity", "room")]
	s.lock.RUnlock()
	require.True(t, ok)
	require.WithinDuration(t, time.Now().Add(24*time.Hour), entry.expiresAt, 5*time.Second)

	// a shorter revocation does not cut the existing one short
	require.NoError(t, s.RevokeRoomParticipant(context.Background(), "room", "identity", time.Now(), time.Minute))
	s.lock.RLock()
	entry = s.tokenRevocationMap[GenRoomParticipantRevocationIdentifier("identity", "room")]
	s.lock.RUnlock()
	require.WithinDuration(t, time.Now().Add(24*time.Hour), entry.expiresAt, 5*time.Second)
}

type revocationCall struct {
	room      livekit.RoomName
	identity  livekit.ParticipantIdentity
	notBefore time.Time
	ttl       time.Duration
}

type recordingRevocationStore struct {
	err   error
	calls []revocationCall
}

func (s *recordingRevocationStore) RevokeRoomParticipant(_ context.Context, room livekit.RoomName, identity livekit.ParticipantIdentity, notBefore time.Time, ttl time.Duration) error {
	if s.err != nil {
		return s.err
	}
	s.calls = append(s.calls, revocationCall{room, identity, notBefore, ttl})
	return nil
}

func (s *recordingRevocationStore) IsRoomParticipantRevoked(context.Context, livekit.ParticipantIdentity, livekit.RoomName) (bool, *time.Time, error) {
	return false, nil, nil
}

func (s *recordingRevocationStore) CleanupRevokedTokens() {}

type revocationRoomServiceTest struct {
	svc               *RoomService
	roomStore         *LocalStore
	revocations       *recordingRevocationStore
	participantClient *rpcfakes.FakeTypedParticipantClient
}

func newRevocationRoomServiceTest(t *testing.T, ttl time.Duration, enabled bool) *revocationRoomServiceTest {
	tt := &revocationRoomServiceTest{
		roomStore:         NewLocalStore(),
		revocations:       &recordingRevocationStore{},
		participantClient: &rpcfakes.FakeTypedParticipantClient{},
	}
	var revokeTokenClient RevokeTokenClient = NilRevokeTokenClient{}
	if enabled {
		revokeTokenClient = newStoreRevokeTokenClient(tt.revocations, ttl)
	}
	limitConf := config.LimitConfig{MaxRoomNameLength: 16, MaxParticipantIdentityLength: 16}
	svc, err := NewRoomService(
		limitConf,
		config.DefaultAPIConfig(),
		&routingfakes.FakeRouter{},
		nil,
		tt.roomStore,
		nil,
		rpc.NewTopicFormatter(),
		&rpcfakes.FakeTypedRoomClient{},
		tt.participantClient,
		revokeTokenClient,
	)
	require.NoError(t, err)
	tt.svc = svc
	return tt
}

func (tt *revocationRoomServiceTest) addParticipant(t *testing.T, identity string) {
	require.NoError(t, tt.roomStore.StoreParticipant(context.Background(), "room", &livekit.ParticipantInfo{Identity: identity}))
}

func revocationAdminContext() context.Context {
	return WithAPIKey(context.Background(), &auth.ClaimGrants{
		Video: &auth.VideoGrant{Room: "room", RoomAdmin: true},
	}, "")
}

func TestRoomServiceRemoveParticipantTokenRevocation(t *testing.T) {
	ctx := revocationAdminContext()
	req := func(identity string, revokeTokenTs int64) *livekit.RoomParticipantIdentity {
		return &livekit.RoomParticipantIdentity{Room: "room", Identity: identity, RevokeTokenTs: revokeTokenTs}
	}

	t.Run("disabled", func(t *testing.T) {
		tt := newRevocationRoomServiceTest(t, 0, false)

		// revoke_token_ts is still validated, but nothing is revoked and not found is kept
		_, err := tt.svc.RemoveParticipant(ctx, req("absent", 1))
		require.ErrorIs(t, err, ErrInvalidRevokeTokenTs)
		_, err = tt.svc.RemoveParticipant(ctx, req("absent", time.Now().Unix()))
		require.ErrorIs(t, err, ErrParticipantNotFound)
		_, err = tt.svc.RemoveParticipant(ctx, req("identity_exceeding_limit", 0))
		require.ErrorIs(t, err, ErrParticipantIdentityExceedsLimits)
		require.Empty(t, tt.revocations.calls)
	})

	t.Run("cutoff defaults to now", func(t *testing.T) {
		tt := newRevocationRoomServiceTest(t, 0, true)
		tt.addParticipant(t, "alice")

		res, err := tt.svc.RemoveParticipant(ctx, req("alice", 0))
		require.NoError(t, err)
		require.NotNil(t, res)
		require.Equal(t, 1, tt.participantClient.RemoveParticipantCallCount())
		require.Len(t, tt.revocations.calls, 1)
		call := tt.revocations.calls[0]
		require.Equal(t, livekit.RoomName("room"), call.room)
		require.Equal(t, livekit.ParticipantIdentity("alice"), call.identity)
		require.WithinDuration(t, time.Now(), call.notBefore, 2*time.Second)
		require.Equal(t, tokenRevocationDefaultTTL, call.ttl)
	})

	t.Run("revoke_token_ts adds the leeway", func(t *testing.T) {
		tt := newRevocationRoomServiceTest(t, 0, true)
		tt.addParticipant(t, "alice")

		ts := time.Now().Unix()
		_, err := tt.svc.RemoveParticipant(ctx, req("alice", ts))
		require.NoError(t, err)
		require.Len(t, tt.revocations.calls, 1)
		require.Equal(t, ts+60, tt.revocations.calls[0].notBefore.Unix())
	})

	t.Run("revoke_token_ts out of range", func(t *testing.T) {
		tt := newRevocationRoomServiceTest(t, 0, true)
		tt.addParticipant(t, "alice")

		for _, ts := range []int64{time.Now().Unix() - 120, time.Now().Unix() + 120} {
			_, err := tt.svc.RemoveParticipant(ctx, req("alice", ts))
			require.ErrorIs(t, err, ErrInvalidRevokeTokenTs)
		}
		require.Zero(t, tt.participantClient.RemoveParticipantCallCount())
		require.Empty(t, tt.revocations.calls)
	})

	t.Run("absent participant is revoked and not found is returned", func(t *testing.T) {
		tt := newRevocationRoomServiceTest(t, 0, true)

		_, err := tt.svc.RemoveParticipant(ctx, req("absent", 0))
		require.ErrorIs(t, err, ErrParticipantNotFound)
		require.Len(t, tt.revocations.calls, 1)
	})

	t.Run("absent participant with revoke_token_ts succeeds", func(t *testing.T) {
		tt := newRevocationRoomServiceTest(t, 0, true)

		res, err := tt.svc.RemoveParticipant(ctx, req("absent", time.Now().Unix()))
		require.NoError(t, err)
		require.NotNil(t, res)
		require.Len(t, tt.revocations.calls, 1)
	})

	t.Run("not found from the participant node is revoked", func(t *testing.T) {
		tt := newRevocationRoomServiceTest(t, 0, true)
		tt.addParticipant(t, "alice")
		tt.participantClient.RemoveParticipantReturns(nil, errors.New(ErrParticipantNotFound.Error()))

		_, err := tt.svc.RemoveParticipant(ctx, req("alice", 0))
		require.Error(t, err)
		require.Len(t, tt.revocations.calls, 1)
	})

	t.Run("other removal errors do not revoke", func(t *testing.T) {
		tt := newRevocationRoomServiceTest(t, 0, true)
		tt.addParticipant(t, "alice")
		tt.participantClient.RemoveParticipantReturns(nil, errors.New("timeout"))

		_, err := tt.svc.RemoveParticipant(ctx, req("alice", 0))
		require.Error(t, err)
		require.Empty(t, tt.revocations.calls)
	})

	t.Run("store failure is returned only with revoke_token_ts", func(t *testing.T) {
		tt := newRevocationRoomServiceTest(t, 0, true)
		tt.addParticipant(t, "alice")
		storeErr := errors.New("store unavailable")
		tt.revocations.err = storeErr

		_, err := tt.svc.RemoveParticipant(ctx, req("alice", 0))
		require.NoError(t, err)

		_, err = tt.svc.RemoveParticipant(ctx, req("alice", time.Now().Unix()))
		require.ErrorIs(t, err, storeErr)
	})

	t.Run("configured ttl", func(t *testing.T) {
		tt := newRevocationRoomServiceTest(t, 48*time.Hour, true)
		tt.addParticipant(t, "alice")

		_, err := tt.svc.RemoveParticipant(ctx, req("alice", 0))
		require.NoError(t, err)
		require.Equal(t, 48*time.Hour, tt.revocations.calls[0].ttl)
	})

	t.Run("length limits", func(t *testing.T) {
		tt := newRevocationRoomServiceTest(t, 0, true)

		_, err := tt.svc.RemoveParticipant(ctx, req("identity_exceeding_limit", 0))
		require.ErrorIs(t, err, ErrParticipantIdentityExceedsLimits)
		require.Empty(t, tt.revocations.calls)
	})
}

func TestRoomServiceUpdateParticipantTokenRevocation(t *testing.T) {
	ctx := revocationAdminContext()

	t.Run("permission update revokes", func(t *testing.T) {
		tt := newRevocationRoomServiceTest(t, 0, true)
		tt.addParticipant(t, "alice")

		_, err := tt.svc.UpdateParticipant(ctx, &livekit.UpdateParticipantRequest{
			Room:       "room",
			Identity:   "alice",
			Permission: &livekit.ParticipantPermission{CanSubscribe: true},
		})
		require.NoError(t, err)
		require.Len(t, tt.revocations.calls, 1)
		require.WithinDuration(t, time.Now(), tt.revocations.calls[0].notBefore, 2*time.Second)
	})

	t.Run("metadata update does not revoke", func(t *testing.T) {
		tt := newRevocationRoomServiceTest(t, 0, true)
		tt.addParticipant(t, "alice")

		_, err := tt.svc.UpdateParticipant(ctx, &livekit.UpdateParticipantRequest{
			Room:     "room",
			Identity: "alice",
			Metadata: "metadata",
		})
		require.NoError(t, err)
		require.Empty(t, tt.revocations.calls)
	})

	t.Run("disabled", func(t *testing.T) {
		tt := newRevocationRoomServiceTest(t, 0, false)
		tt.addParticipant(t, "alice")

		_, err := tt.svc.UpdateParticipant(ctx, &livekit.UpdateParticipantRequest{
			Room:       "room",
			Identity:   "alice",
			Permission: &livekit.ParticipantPermission{CanSubscribe: true},
		})
		require.NoError(t, err)
		require.Empty(t, tt.revocations.calls)
	})

}
