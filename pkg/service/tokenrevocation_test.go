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

package service_test

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/auth/authfakes"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/utils/guid"

	"github.com/livekit/livekit-server/pkg/agent/endpoint"
	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/service"
)

const (
	revocationTestAPIKey = "APIabcdefg"
	revocationTestSecret = "somesecretencodedinbase62extendto32bytes"
	revocationTestRoom   = "revocation_room"
	revocationTestTTL    = time.Hour
)

type stubTokenRevocationStore struct {
	checkErr error
}

func (s *stubTokenRevocationStore) RevokeRoomParticipant(context.Context, livekit.RoomName, livekit.ParticipantIdentity, time.Time, time.Duration) error {
	return nil
}

func (s *stubTokenRevocationStore) IsRoomParticipantRevoked(context.Context, livekit.ParticipantIdentity, livekit.RoomName) (bool, *time.Time, error) {
	return false, nil, s.checkErr
}

// tokens whose nbf is earlier than notBefore minus the one minute leeway are revoked
func revokeForTest(t *testing.T, store service.TokenRevocationStore, room string, identity string, notBefore time.Time) {
	require.NoError(t, store.RevokeRoomParticipant(context.Background(), livekit.RoomName(room), livekit.ParticipantIdentity(identity), notBefore, revocationTestTTL))
}

func newRevocationTestToken(t *testing.T, identity string, grant *auth.VideoGrant) string {
	token, err := auth.NewAccessToken(revocationTestAPIKey, revocationTestSecret).
		SetIdentity(identity).
		SetVideoGrant(grant).
		ToJWT()
	require.NoError(t, err)
	return token
}

func serveWithRevocationStore(t *testing.T, store service.TokenRevocationStore, target string, token string, next http.Handler) (int, *auth.ClaimGrants) {
	provider := &authfakes.FakeKeyProvider{}
	provider.GetSecretReturns(revocationTestSecret)

	var grants *auth.ClaimGrants
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		grants = service.GetGrants(r.Context())
		if next != nil {
			next.ServeHTTP(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	r := httptest.NewRequest(http.MethodGet, target, nil)
	service.SetAuthorizationToken(r, token)
	w := httptest.NewRecorder()
	service.NewAPIKeyAuthMiddleware(provider, store).ServeHTTP(w, r, handler)
	return w.Code, grants
}

func TestGenRoomParticipantRevocationIdentifier(t *testing.T) {
	id := service.GenRoomParticipantRevocationIdentifier("identity", "room")
	require.Equal(t, "token_revocation:room:room:participant:identity", id)

	// separators inside names must not cause collisions
	require.NotEqual(t,
		service.GenRoomParticipantRevocationIdentifier("b:participant:c", "a"),
		service.GenRoomParticipantRevocationIdentifier("c", "a:participant:b"),
	)
}

func testTokenRevocationStore(t *testing.T, store service.TokenRevocationStore) {
	ctx := context.Background()
	room := livekit.RoomName(guid.New("room_"))
	identity := livekit.ParticipantIdentity(guid.New("identity_"))

	revoked, _, err := store.IsRoomParticipantRevoked(ctx, identity, room)
	require.NoError(t, err)
	require.False(t, revoked)

	revokedAt := time.Unix(time.Now().Unix(), 0)
	revokeForTest(t, store, string(room), string(identity), revokedAt)

	revoked, storedRevokedAt, err := store.IsRoomParticipantRevoked(ctx, identity, room)
	require.NoError(t, err)
	require.True(t, revoked)
	require.NotNil(t, storedRevokedAt)
	require.Equal(t, revokedAt.Unix(), storedRevokedAt.Unix())

	// revocation is scoped to the room and identity
	revoked, _, err = store.IsRoomParticipantRevoked(ctx, identity+"_other", room)
	require.NoError(t, err)
	require.False(t, revoked)

	revoked, _, err = store.IsRoomParticipantRevoked(ctx, identity, room+"_other")
	require.NoError(t, err)
	require.False(t, revoked)

	// revoking again moves the revocation time forward
	revokeForTest(t, store, string(room), string(identity), revokedAt.Add(30*time.Second))

	revoked, storedRevokedAt, err = store.IsRoomParticipantRevoked(ctx, identity, room)
	require.NoError(t, err)
	require.True(t, revoked)
	require.Equal(t, revokedAt.Add(30*time.Second).Unix(), storedRevokedAt.Unix())

	// but never back
	revokeForTest(t, store, string(room), string(identity), revokedAt)

	revoked, storedRevokedAt, err = store.IsRoomParticipantRevoked(ctx, identity, room)
	require.NoError(t, err)
	require.True(t, revoked)
	require.Equal(t, revokedAt.Add(30*time.Second).Unix(), storedRevokedAt.Unix())
}

func TestLocalStoreTokenRevocation(t *testing.T) {
	testTokenRevocationStore(t, service.NewLocalStore())
}

func TestLocalStoreTokenRevocationConcurrent(t *testing.T) {
	ctx := context.Background()
	store := service.NewLocalStore()

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			identity := fmt.Sprintf("identity_%d", i)
			for j := 0; j < 100; j++ {
				_ = store.RevokeRoomParticipant(ctx, "room", livekit.ParticipantIdentity(identity), time.Now(), revocationTestTTL)
				_, _, _ = store.IsRoomParticipantRevoked(ctx, livekit.ParticipantIdentity(identity), "room")
			}
		}(i)
	}
	wg.Wait()

	for i := 0; i < 20; i++ {
		revoked, _, err := store.IsRoomParticipantRevoked(ctx, livekit.ParticipantIdentity(fmt.Sprintf("identity_%d", i)), "room")
		require.NoError(t, err)
		require.True(t, revoked)
	}
}

func TestRedisStoreTokenRevocation(t *testing.T) {
	testTokenRevocationStore(t, redisStore(t))
}

// The revocation TTL can be raised above the validity of the tokens handed out to participants.
func TestTokenRevocationTTLConfig(t *testing.T) {
	conf, err := config.NewConfig("", true, nil, nil)
	require.NoError(t, err)
	require.False(t, conf.TokenRevocation.Enabled)
	require.Zero(t, conf.TokenRevocation.TTL, "unset, the room service falls back to its default")

	conf, err = config.NewConfig("token_revocation:\n  enabled: true\n  ttl: 48h\n", true, nil, nil)
	require.NoError(t, err)
	require.True(t, conf.TokenRevocation.Enabled)
	require.Equal(t, 48*time.Hour, conf.TokenRevocation.TTL)
}

func TestRedisStoreTokenRevocationTTL(t *testing.T) {
	ctx := context.Background()
	cli := redisClient(t)
	store := service.NewRedisStore(cli)

	room := livekit.RoomName(guid.New("room_"))
	identity := livekit.ParticipantIdentity(guid.New("identity_"))
	revokeForTest(t, store, string(room), string(identity), time.Now())

	ttl, err := cli.TTL(ctx, service.GenRoomParticipantRevocationIdentifier(identity, room)).Result()
	require.NoError(t, err)
	require.InDelta(t, revocationTestTTL.Seconds(), ttl.Seconds(), 5)
}

func TestRedisStoreTokenRevocationReportsErrors(t *testing.T) {
	cli := redis.NewClient(&redis.Options{
		Addr:        "127.0.0.1:1",
		DialTimeout: 100 * time.Millisecond,
		MaxRetries:  -1,
	})
	t.Cleanup(func() { _ = cli.Close() })
	store := service.NewRedisStore(cli)

	_, _, err := store.IsRoomParticipantRevoked(context.Background(), "identity", "room")
	require.Error(t, err)

	err = store.RevokeRoomParticipant(context.Background(), "room", "identity", time.Now(), revocationTestTTL)
	require.Error(t, err)
}

func TestAuthMiddlewareTokenRevocation(t *testing.T) {
	grant := &auth.VideoGrant{Room: revocationTestRoom, RoomJoin: true}

	t.Run("not revoked", func(t *testing.T) {
		code, grants := serveWithRevocationStore(t, service.NewLocalStore(), "/rtc", newRevocationTestToken(t, "alice", grant), nil)
		require.Equal(t, http.StatusOK, code)
		require.NotNil(t, grants)
		require.Equal(t, "alice", grants.Identity)
	})

	t.Run("token issued before revocation is rejected", func(t *testing.T) {
		store := service.NewLocalStore()
		token := newRevocationTestToken(t, "alice", grant)
		revokeForTest(t, store, revocationTestRoom, "alice", time.Now().Add(2*time.Minute))

		code, grants := serveWithRevocationStore(t, store, "/rtc", token, nil)
		require.Equal(t, http.StatusUnauthorized, code)
		require.Nil(t, grants)
	})

	t.Run("token issued within the leeway of the revocation is accepted", func(t *testing.T) {
		store := service.NewLocalStore()
		revokeForTest(t, store, revocationTestRoom, "alice", time.Now())

		code, grants := serveWithRevocationStore(t, store, "/rtc", newRevocationTestToken(t, "alice", grant), nil)
		require.Equal(t, http.StatusOK, code)
		require.NotNil(t, grants)
	})

	t.Run("other participants are not affected", func(t *testing.T) {
		store := service.NewLocalStore()
		revokeForTest(t, store, revocationTestRoom, "alice", time.Now().Add(2*time.Minute))

		code, grants := serveWithRevocationStore(t, store, "/rtc", newRevocationTestToken(t, "bob", grant), nil)
		require.Equal(t, http.StatusOK, code)
		require.NotNil(t, grants)
	})
}

// When the revocation state cannot be read, the token must not be accepted.
func TestAuthMiddlewareTokenRevocationStoreFailure(t *testing.T) {
	store := &stubTokenRevocationStore{checkErr: errors.New("store unavailable")}
	token := newRevocationTestToken(t, "alice", &auth.VideoGrant{Room: revocationTestRoom, RoomJoin: true})

	code, grants := serveWithRevocationStore(t, store, "/rtc", token, nil)
	require.Equal(t, http.StatusServiceUnavailable, code)
	require.Nil(t, grants)
}

// The stored cutoff is revoke_token_ts plus the leeway, so tokens whose nbf is before revoke_token_ts are revoked.
func TestAuthMiddlewareTokenRevocationBoundary(t *testing.T) {
	token := newRevocationTestToken(t, "alice", &auth.VideoGrant{Room: revocationTestRoom, RoomJoin: true})

	v, err := auth.ParseAPIToken(token)
	require.NoError(t, err)
	claims, _, err := v.Verify(revocationTestSecret)
	require.NoError(t, err)
	require.NotNil(t, claims.NotBefore)
	nbf := claims.NotBefore.Time

	t.Run("nbf before revoke_token_ts is rejected", func(t *testing.T) {
		store := service.NewLocalStore()
		revokeForTest(t, store, revocationTestRoom, "alice", nbf.Add(time.Minute+time.Second))

		code, grants := serveWithRevocationStore(t, store, "/rtc", token, nil)
		require.Equal(t, http.StatusUnauthorized, code)
		require.Nil(t, grants)
	})

	t.Run("nbf at revoke_token_ts is accepted", func(t *testing.T) {
		store := service.NewLocalStore()
		revokeForTest(t, store, revocationTestRoom, "alice", nbf.Add(time.Minute))

		code, grants := serveWithRevocationStore(t, store, "/rtc", token, nil)
		require.Equal(t, http.StatusOK, code)
		require.NotNil(t, grants)
	})
}

func TestAuthMiddlewareTokenRevocationWithoutNotBefore(t *testing.T) {
	now := time.Now()
	newToken := func(t *testing.T, issuedAt *jwt.NumericDate) string {
		claims := struct {
			jwt.RegisteredClaims
			auth.ClaimGrants
		}{
			RegisteredClaims: jwt.RegisteredClaims{
				Issuer:    revocationTestAPIKey,
				Subject:   "alice",
				IssuedAt:  issuedAt,
				ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
			},
			ClaimGrants: auth.ClaimGrants{
				Identity: "alice",
				Video:    &auth.VideoGrant{Room: revocationTestRoom, RoomJoin: true},
			},
		}
		token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(revocationTestSecret))
		require.NoError(t, err)
		return token
	}

	for name, tc := range map[string]struct {
		issuedAt *jwt.NumericDate
		accepted bool
	}{
		"iat before revocation": {issuedAt: jwt.NewNumericDate(now.Add(-time.Hour))},
		"iat after revocation":  {issuedAt: jwt.NewNumericDate(now.Add(-time.Minute)), accepted: true},
		"without iat":           {},
	} {
		t.Run(name, func(t *testing.T) {
			token := newToken(t, tc.issuedAt)
			store := service.NewLocalStore()

			// the token itself is valid
			code, grants := serveWithRevocationStore(t, store, "/rtc", token, nil)
			require.Equal(t, http.StatusOK, code)
			require.NotNil(t, grants)

			revokeForTest(t, store, revocationTestRoom, "alice", now.Add(-30*time.Minute))

			code, grants = serveWithRevocationStore(t, store, "/rtc", token, nil)
			if tc.accepted {
				require.Equal(t, http.StatusOK, code)
				require.NotNil(t, grants)
			} else {
				require.Equal(t, http.StatusUnauthorized, code)
				require.Nil(t, grants)
			}
		})
	}
}

// every chain with api-key auth checks revocation: the API, the agent endpoints and WebTransport
func TestAuthChainsTokenRevocation(t *testing.T) {
	conf, err := config.NewConfig("", true, nil, nil)
	require.NoError(t, err)
	provider := &authfakes.FakeKeyProvider{}
	provider.GetSecretReturns(revocationTestSecret)
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	token := newRevocationTestToken(t, "alice", &auth.VideoGrant{Room: revocationTestRoom, RoomJoin: true})

	for name, tc := range map[string]struct {
		handler func(store service.TokenRevocationStore) http.Handler
		path    string
	}{
		"api": {
			handler: func(store service.TokenRevocationStore) http.Handler {
				return service.NewHTTPHandler(conf, provider, store, ok, ok)
			},
			path: "/rtc/validate",
		},
		"agent endpoint": {
			handler: func(store service.TokenRevocationStore) http.Handler {
				return service.NewHTTPHandler(conf, provider, store, ok, ok)
			},
			path: endpoint.PathPrefix + "agent",
		},
		"webtransport": {
			handler: func(store service.TokenRevocationStore) http.Handler {
				return service.NewWebTransportHandler(provider, store, service.NewWebTransportServer(&tls.Config{}), ok)
			},
			path: "/rtc",
		},
	} {
		t.Run(name, func(t *testing.T) {
			store := service.NewLocalStore()
			handler := tc.handler(store)
			serve := func() int {
				r := httptest.NewRequest(http.MethodGet, tc.path, nil)
				service.SetAuthorizationToken(r, token)
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, r)
				return w.Code
			}

			require.Equal(t, http.StatusOK, serve())
			revokeForTest(t, store, revocationTestRoom, "alice", time.Now().Add(2*time.Minute))
			require.Equal(t, http.StatusUnauthorized, serve())
		})
	}
}
