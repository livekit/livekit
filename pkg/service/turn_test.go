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

	"github.com/jxskiss/base62"
	"github.com/pion/stun/v3"
	"github.com/pion/turn/v5"
	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
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

	username, _ := h.CreateUsername(turnTestAPIKey, pID, -60)
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

	username, expiry := h.CreateUsername(turnTestAPIKey, pID, -60)

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
