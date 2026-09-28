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
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/rtc/types/typesfakes"
)

const (
	testRevocationRoom     = livekit.RoomName("revocation-room")
	testRevocationIdentity = livekit.ParticipantIdentity("revocation-user")
)

func TestLocalStoreTokenRevocation(t *testing.T) {
	ctx := context.Background()

	newStore := func(start time.Time) (*LocalStore, *time.Time) {
		now := start
		s := NewLocalStore()
		s.now = func() time.Time { return now }
		return s, &now
	}
	base := time.Unix(1_700_000_000, 0)

	t.Run("absent identity returns zero cutoff", func(t *testing.T) {
		s, _ := newStore(base)
		cutoff, err := s.GetRevocationCutoff(ctx, testRevocationRoom, testRevocationIdentity)
		require.NoError(t, err)
		require.True(t, cutoff.IsZero())
	})

	t.Run("roundtrip", func(t *testing.T) {
		s, _ := newStore(base)
		require.NoError(t, s.RevokeTokensBefore(ctx, testRevocationRoom, testRevocationIdentity, base, time.Hour))
		cutoff, err := s.GetRevocationCutoff(ctx, testRevocationRoom, testRevocationIdentity)
		require.NoError(t, err)
		require.True(t, cutoff.Equal(base))

		// other identity and other room unaffected
		cutoff, err = s.GetRevocationCutoff(ctx, testRevocationRoom, "other-user")
		require.NoError(t, err)
		require.True(t, cutoff.IsZero())
		cutoff, err = s.GetRevocationCutoff(ctx, "other-room", testRevocationIdentity)
		require.NoError(t, err)
		require.True(t, cutoff.IsZero())
	})

	t.Run("cutoff only moves forward", func(t *testing.T) {
		s, _ := newStore(base)
		later := base.Add(time.Minute)
		require.NoError(t, s.RevokeTokensBefore(ctx, testRevocationRoom, testRevocationIdentity, later, time.Hour))
		require.NoError(t, s.RevokeTokensBefore(ctx, testRevocationRoom, testRevocationIdentity, base, time.Hour))
		cutoff, err := s.GetRevocationCutoff(ctx, testRevocationRoom, testRevocationIdentity)
		require.NoError(t, err)
		require.True(t, cutoff.Equal(later))
	})

	t.Run("entries expire after ttl", func(t *testing.T) {
		s, now := newStore(base)
		require.NoError(t, s.RevokeTokensBefore(ctx, testRevocationRoom, testRevocationIdentity, base, time.Hour))

		*now = base.Add(59 * time.Minute)
		cutoff, err := s.GetRevocationCutoff(ctx, testRevocationRoom, testRevocationIdentity)
		require.NoError(t, err)
		require.False(t, cutoff.IsZero())

		*now = base.Add(61 * time.Minute)
		cutoff, err = s.GetRevocationCutoff(ctx, testRevocationRoom, testRevocationIdentity)
		require.NoError(t, err)
		require.True(t, cutoff.IsZero())
	})

	t.Run("writes prune expired entries", func(t *testing.T) {
		s, now := newStore(base)
		require.NoError(t, s.RevokeTokensBefore(ctx, testRevocationRoom, "expired-user", base, time.Minute))
		*now = base.Add(2 * time.Minute)
		require.NoError(t, s.RevokeTokensBefore(ctx, testRevocationRoom, testRevocationIdentity, *now, time.Hour))
		require.Len(t, s.tokenCutoffs, 1)
	})
}

type stubRevocationStore struct {
	cutoff time.Time
	ttl    time.Duration
	err    error
}

func (s *stubRevocationStore) RevokeTokensBefore(_ context.Context, _ livekit.RoomName, _ livekit.ParticipantIdentity, cutoff time.Time, ttl time.Duration) error {
	s.cutoff = cutoff
	s.ttl = ttl
	return s.err
}

func (s *stubRevocationStore) GetRevocationCutoff(_ context.Context, _ livekit.RoomName, _ livekit.ParticipantIdentity) (time.Time, error) {
	return s.cutoff, s.err
}

func ctxWithNotBefore(claims *auth.ClaimGrants, notBefore time.Time) context.Context {
	return context.WithValue(context.Background(), grantsKey{}, &grantsValue{
		claims:    claims,
		notBefore: notBefore,
	})
}

func TestEnsureTokenNotRevoked(t *testing.T) {
	cutoff := time.Unix(1_700_000_000, 0)
	storeErr := errors.New("store unavailable")

	cases := []struct {
		name      string
		store     TokenRevocationStore
		notBefore time.Time
		want      error
	}{
		{"nil store means disabled", nil, cutoff.Add(-time.Hour), nil},
		{"no cutoff recorded", &stubRevocationStore{}, cutoff.Add(-time.Hour), nil},
		{"nbf before cutoff rejected", &stubRevocationStore{cutoff: cutoff}, cutoff.Add(-time.Second), ErrTokenRevoked},
		{"nbf equal to cutoff passes", &stubRevocationStore{cutoff: cutoff}, cutoff, nil},
		{"nbf after cutoff passes", &stubRevocationStore{cutoff: cutoff}, cutoff.Add(time.Second), nil},
		{"missing nbf and iat fails closed", &stubRevocationStore{cutoff: cutoff}, time.Time{}, ErrTokenRevoked},
		{"store error fails closed", &stubRevocationStore{err: storeErr}, cutoff, storeErr},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := ctxWithNotBefore(nil, tc.notBefore)
			err := EnsureTokenNotRevoked(ctx, tc.store, testRevocationRoom, testRevocationIdentity)
			if tc.want == nil {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, tc.want)
			}
		})
	}
}

func TestRevokeParticipantTokens(t *testing.T) {
	newParticipant := func(expiresAt time.Time) *typesfakes.FakeLocalParticipant {
		p := &typesfakes.FakeLocalParticipant{}
		p.IdentityReturns(testRevocationIdentity)
		p.TokenExpiresAtReturns(expiresAt)
		p.GetLoggerReturns(logger.GetLogger())
		return p
	}
	ctx := context.Background()

	t.Run("nil store is a no-op", func(t *testing.T) {
		r := &RoomManager{config: &config.Config{}}
		require.NoError(t, r.revokeParticipantTokens(ctx, testRevocationRoom, newParticipant(time.Time{}), true))
	})

	t.Run("store errors propagate so callers fail closed", func(t *testing.T) {
		storeErr := errors.New("store unavailable")
		r := &RoomManager{config: &config.Config{}, tokenRevocation: &stubRevocationStore{err: storeErr}}
		require.ErrorIs(t, r.revokeParticipantTokens(ctx, testRevocationRoom, newParticipant(time.Time{}), true), storeErr)
	})

	t.Run("cutoff and ttl", func(t *testing.T) {
		store := &stubRevocationStore{}
		r := &RoomManager{config: &config.Config{}, tokenRevocation: store}

		// includeCurrent covers the current second
		before := time.Now()
		require.NoError(t, r.revokeParticipantTokens(ctx, testRevocationRoom, newParticipant(time.Time{}), true))
		require.True(t, store.cutoff.After(before))
		// unset retention falls back to the default
		require.GreaterOrEqual(t, store.ttl, tokenRevocationDefaultRetention)

		// without includeCurrent the cutoff is floored to the current second
		require.NoError(t, r.revokeParticipantTokens(ctx, testRevocationRoom, newParticipant(time.Time{}), false))
		require.False(t, store.cutoff.After(time.Now()))
	})

	t.Run("configured retention", func(t *testing.T) {
		store := &stubRevocationStore{}
		conf := &config.Config{}
		conf.Room.TokenRevocationRetention = 30 * time.Minute
		r := &RoomManager{config: conf, tokenRevocation: store}

		// short-lived token: the configured retention is the floor
		require.NoError(t, r.revokeParticipantTokens(ctx, testRevocationRoom, newParticipant(time.Time{}), true))
		require.Equal(t, 30*time.Minute, store.ttl)

		// the participant's token outliving the retention extends it
		expiry := time.Now().Add(2 * time.Hour)
		require.NoError(t, r.revokeParticipantTokens(ctx, testRevocationRoom, newParticipant(expiry), true))
		require.Greater(t, store.ttl, 2*time.Hour)
	})
}

func TestGetTokenRevocationStore(t *testing.T) {
	conf := &config.Config{}

	// disabled: no store, no error, whatever the ObjectStore is
	store, err := getTokenRevocationStore(nil, conf)
	require.NoError(t, err)
	require.Nil(t, store)

	conf.Room.TokenRevocation = true

	store, err = getTokenRevocationStore(NewLocalStore(), conf)
	require.NoError(t, err)
	require.NotNil(t, store)

	// enabled on a store that cannot record revocations must fail startup
	// instead of silently disabling the feature
	_, err = getTokenRevocationStore(nil, conf)
	require.Error(t, err)
}

type stubRoomAllocator struct{}

func (a stubRoomAllocator) AutoCreateEnabled(context.Context) bool { return true }
func (a stubRoomAllocator) SelectRoomNode(context.Context, livekit.RoomName, livekit.NodeID) error {
	return nil
}
func (a stubRoomAllocator) CreateRoom(context.Context, *livekit.CreateRoomRequest, bool) (*livekit.Room, *livekit.RoomInternal, bool, error) {
	return nil, nil, false, nil
}
func (a stubRoomAllocator) ValidateCreateRoom(context.Context, livekit.RoomName) error { return nil }

func TestValidateConnectRequestTokenRevocation(t *testing.T) {
	store := NewLocalStore()
	claims := &auth.ClaimGrants{
		Identity: string(testRevocationIdentity),
		Video:    &auth.VideoGrant{RoomJoin: true, Room: string(testRevocationRoom)},
	}
	nbf := time.Now()

	validate := func(notBefore time.Time) (int, error) {
		r := httptest.NewRequest("GET", "/rtc/validate", nil)
		r = r.WithContext(ctxWithNotBefore(claims, notBefore))
		_, code, err := ValidateConnectRequest(
			logger.GetLogger(),
			r,
			config.LimitConfig{},
			ValidateConnectRequestParams{},
			nil,
			stubRoomAllocator{},
			store,
		)
		return code, err
	}

	code, err := validate(nbf)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, code)

	// revoke everything issued before one hour from now
	require.NoError(t, store.RevokeTokensBefore(context.Background(), testRevocationRoom, testRevocationIdentity, nbf.Add(time.Hour), time.Hour))

	code, err = validate(nbf)
	require.ErrorIs(t, err, ErrTokenRevoked)
	require.Equal(t, http.StatusUnauthorized, code)

	// tokens issued after the cutoff pass again
	code, err = validate(nbf.Add(2 * time.Hour))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, code)

	// publish-only connections join as "<identity>#<suffix>"; a revocation
	// recorded against the suffixed identity must block them even though the
	// base identity has no (effective) cutoff
	canPublish := true
	validatePublish := func(notBefore time.Time) (int, error) {
		claims := &auth.ClaimGrants{
			Identity: string(testRevocationIdentity),
			Video:    &auth.VideoGrant{RoomJoin: true, Room: string(testRevocationRoom), CanPublish: &canPublish},
		}
		r := httptest.NewRequest("GET", "/rtc/validate", nil)
		r = r.WithContext(ctxWithNotBefore(claims, notBefore))
		_, code, err := ValidateConnectRequest(
			logger.GetLogger(),
			r,
			config.LimitConfig{},
			ValidateConnectRequestParams{publish: "screen"},
			nil,
			stubRoomAllocator{},
			store,
		)
		return code, err
	}

	code, err = validatePublish(nbf.Add(2 * time.Hour))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, code)

	require.NoError(t, store.RevokeTokensBefore(
		context.Background(), testRevocationRoom, testRevocationIdentity+"#screen", nbf.Add(3*time.Hour), time.Hour,
	))

	code, err = validatePublish(nbf.Add(2 * time.Hour))
	require.ErrorIs(t, err, ErrTokenRevoked)
	require.Equal(t, http.StatusUnauthorized, code)
}
