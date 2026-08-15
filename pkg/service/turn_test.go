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
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/jxskiss/base62"
	"github.com/pion/stun/v3"
	"github.com/pion/turn/v5"
	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"

	"github.com/livekit/livekit-server/pkg/config"
)

const (
	turnTestAPIKey    = "APITestKey"
	turnTestAPISecret = "TestSecret"
)

func newTestTurnAuthHandler() *TURNAuthHandler {
	return NewTURNAuthHandler(auth.NewSimpleKeyProvider(turnTestAPIKey, turnTestAPISecret))
}

func mustAuthCreds(t *testing.T, h *TURNAuthHandler, pID livekit.ParticipantID, ttlSeconds int) (username string, key []byte) {
	t.Helper()
	username, expiry := h.CreateUsername(turnTestAPIKey, pID, ttlSeconds)
	password, err := h.CreatePassword(turnTestAPIKey, pID, expiry)
	require.NoError(t, err)
	return username, turn.GenerateAuthKey(username, LivekitRealm, password)
}

func TestTURNAuthHandler_HandleAuth_ValidCredentials(t *testing.T) {
	h := newTestTurnAuthHandler()
	pID := livekit.ParticipantID("PA_valid")
	username, expectedKey := mustAuthCreds(t, h, pID, 300)

	for _, method := range []stun.Method{
		stun.MethodAllocate,
		stun.MethodRefresh,
		stun.MethodCreatePermission,
		stun.MethodChannelBind,
		stun.MethodSend,
	} {
		t.Run(method.String(), func(t *testing.T) {
			userID, key, ok := h.HandleAuth(&turn.RequestAttributes{
				Username: username,
				Realm:    LivekitRealm,
				SrcAddr:  &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1234},
				Method:   method,
			})
			require.True(t, ok)
			require.Equal(t, string(pID), userID)
			require.Equal(t, expectedKey, key)
		})
	}
}

func TestTURNAuthHandler_HandleAuth_ExpiredAllocateRejected(t *testing.T) {
	h := newTestTurnAuthHandler()
	pID := livekit.ParticipantID("PA_expired_alloc")

	expiry := time.Now().Add(-time.Minute).Unix()
	username := base62.EncodeToString(fmt.Appendf(nil, "%s|%s|%d", turnTestAPIKey, pID, expiry))
	_, _, ok := h.HandleAuth(&turn.RequestAttributes{
		Username: username,
		Realm:    LivekitRealm,
		SrcAddr:  &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1234},
		Method:   stun.MethodAllocate,
	})
	require.False(t, ok, "Allocate request with expired credentials must be rejected")
}

func TestTURNAuthHandler_HandleAuth_ExpiredNonAllocateAllowed(t *testing.T) {
	h := newTestTurnAuthHandler()
	pID := livekit.ParticipantID("PA_expired_refresh")

	expiry := time.Now().Add(-time.Minute).Unix()
	username := base62.EncodeToString(fmt.Appendf(nil, "%s|%s|%d", turnTestAPIKey, pID, expiry))

	// CreatePassword still enforces ErrExpired on its own, but the server hands
	// the same key it generated at allocation time — reproduce that by directly
	// hashing without going through CreatePassword's expiry guard.
	password, err := h.computePassword(turnTestAPIKey, pID, expiry)
	require.NoError(t, err)
	expectedKey := turn.GenerateAuthKey(username, LivekitRealm, password)

	for _, method := range []stun.Method{
		stun.MethodRefresh,
		stun.MethodCreatePermission,
		stun.MethodChannelBind,
		stun.MethodSend,
	} {
		t.Run(method.String(), func(t *testing.T) {
			userID, key, ok := h.HandleAuth(&turn.RequestAttributes{
				Username: username,
				Realm:    LivekitRealm,
				SrcAddr:  &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1234},
				Method:   method,
			})
			require.True(t, ok, "Non-allocate request with expired credentials must succeed")
			require.Equal(t, string(pID), userID)
			require.Equal(t, expectedKey, key)
		})
	}
}

func TestTURNAuthHandler_HandleAuth_WrongUsernameRejected(t *testing.T) {
	h := newTestTurnAuthHandler()
	_, _, ok := h.HandleAuth(&turn.RequestAttributes{
		Username: "not-base62!!!",
		Realm:    LivekitRealm,
		SrcAddr:  &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1234},
		Method:   stun.MethodRefresh,
	})
	require.False(t, ok)
}

func TestTURNAuthHandler_HandleAuth_TwoPartUsernameRejected(t *testing.T) {
	h := newTestTurnAuthHandler()
	pID := livekit.ParticipantID("PA_two_part")

	username := base62.EncodeToString(fmt.Appendf(nil, "%s|%s", turnTestAPIKey, pID))

	for _, method := range []stun.Method{
		stun.MethodAllocate,
		stun.MethodRefresh,
		stun.MethodCreatePermission,
		stun.MethodChannelBind,
		stun.MethodSend,
	} {
		t.Run(method.String(), func(t *testing.T) {
			_, _, ok := h.HandleAuth(&turn.RequestAttributes{
				Username: username,
				Realm:    LivekitRealm,
				SrcAddr:  &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1234},
				Method:   method,
			})
			require.False(t, ok, "Two-part username must be rejected")
		})
	}
}

func TestTURNAuthHandler_HandleAuth_ZeroExpiryRejected(t *testing.T) {
	h := newTestTurnAuthHandler()
	pID := livekit.ParticipantID("PA_zero_expiry")

	username := base62.EncodeToString(fmt.Appendf(nil, "%s|%s|%d", turnTestAPIKey, pID, 0))

	for _, method := range []stun.Method{
		stun.MethodAllocate,
		stun.MethodRefresh,
		stun.MethodCreatePermission,
		stun.MethodChannelBind,
		stun.MethodSend,
	} {
		t.Run(method.String(), func(t *testing.T) {
			_, _, ok := h.HandleAuth(&turn.RequestAttributes{
				Username: username,
				Realm:    LivekitRealm,
				SrcAddr:  &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1234},
				Method:   method,
			})
			require.False(t, ok, "Username with expiry=0 must be rejected")
		})
	}
}

func TestTURNAuthHandler_ParseUsername_TwoPartRejected(t *testing.T) {
	h := newTestTurnAuthHandler()
	pID := livekit.ParticipantID("PA_parse_two_part")

	username := base62.EncodeToString(fmt.Appendf(nil, "%s|%s", turnTestAPIKey, pID))

	_, _, _, err := h.ParseUsername(username)
	require.Error(t, err)
}

func TestTURNAuthHandler_ParseUsername_ZeroExpiryRejected(t *testing.T) {
	h := newTestTurnAuthHandler()
	pID := livekit.ParticipantID("PA_parse_zero_expiry")

	username := base62.EncodeToString(fmt.Appendf(nil, "%s|%s|%d", turnTestAPIKey, pID, 0))

	_, _, _, err := h.ParseUsername(username)
	require.ErrorIs(t, err, ErrExpired)
}

func TestTURNAuthHandler_CreatePassword_ZeroExpiryRejected(t *testing.T) {
	h := newTestTurnAuthHandler()
	pID := livekit.ParticipantID("PA_password_zero_expiry")

	_, err := h.CreatePassword(turnTestAPIKey, pID, 0)
	require.ErrorIs(t, err, ErrExpired)
}

func TestParsePeerCIDRs(t *testing.T) {
	t.Run("valid entries are compiled", func(t *testing.T) {
		nets, err := parsePeerCIDRs("turn.deny_peer_cidrs", []string{"203.0.113.0/24", "10.0.0.0/8"})
		require.NoError(t, err)
		require.Len(t, nets, 2)
		require.True(t, nets[0].Contains(net.ParseIP("203.0.113.5")))
		require.False(t, nets[0].Contains(net.ParseIP("203.0.114.5")))
	})

	t.Run("empty list is fine", func(t *testing.T) {
		nets, err := parsePeerCIDRs("turn.deny_peer_cidrs", nil)
		require.NoError(t, err)
		require.Empty(t, nets)
	})

	t.Run("invalid entry is rejected with field context", func(t *testing.T) {
		_, err := parsePeerCIDRs("turn.deny_peer_cidrs", []string{"203.0.113.0/33"})
		require.Error(t, err)
		require.Contains(t, err.Error(), "turn.deny_peer_cidrs")
		require.Contains(t, err.Error(), "203.0.113.0/33")
	})
}

func TestNewTurnServer_InvalidPeerCIDRFailsStartup(t *testing.T) {
	for _, tc := range []struct {
		name  string
		mutID func(c *config.Config)
		field string
	}{
		{
			name:  "invalid deny cidr",
			mutID: func(c *config.Config) { c.TURN.DenyPeerCIDRs = []string{"203.0.113.0/33"} },
			field: "turn.deny_peer_cidrs",
		},
		{
			name:  "invalid allow cidr",
			mutID: func(c *config.Config) { c.TURN.AllowRestrictedPeerCIDRs = []string{"not-a-cidr"} },
			field: "turn.allow_restricted_peer_cidrs",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conf := &config.Config{}
			conf.TURN.Enabled = true
			conf.TURN.UDPPort = 3478
			tc.mutID(conf)

			_, err := NewTurnServer(conf, nil, false)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.field)
		})
	}
}

func TestNewTurnServer_UDPUseDomainRequiresValidDomain(t *testing.T) {
	for _, tc := range []struct {
		name   string
		domain string
	}{
		{name: "missing domain"},
		{name: "invalid domain", domain: "not a domain"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conf := &config.Config{}
			conf.TURN.Enabled = true
			conf.TURN.UDPPort = 3478
			conf.TURN.UDPUseDomain = true
			conf.TURN.Domain = tc.domain

			_, err := NewTurnServer(conf, nil, false)
			require.Error(t, err)
		})
	}
}

func TestTURNAuthHandler_CreateUsername_TTLClamped(t *testing.T) {
	h := newTestTurnAuthHandler()
	pID := livekit.ParticipantID("PA_ttl_clamp")

	// An overflowing TTL must not wrap into a past expiry; it clamps to the max.
	_, overflowExpiry := h.CreateUsername(turnTestAPIKey, pID, 1<<62+1)
	require.Greater(t, overflowExpiry, time.Now().Unix())
	require.LessOrEqual(t, overflowExpiry, time.Now().Unix()+int64(config.TURNMaxTTLSeconds)+1)

	// A non-positive TTL falls back to the default rather than producing a past/wrapped expiry.
	_, negativeExpiry := h.CreateUsername(turnTestAPIKey, pID, -1<<40)
	require.InDelta(t, time.Now().Unix()+int64(config.DefaultTURNTTLSeconds), negativeExpiry, 2)
}
