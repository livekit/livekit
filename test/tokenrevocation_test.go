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

package test

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/livekit"

	"github.com/livekit/livekit-server/pkg/config"
	testclient "github.com/livekit/livekit-server/test/client"
)

func enableTokenRevocation(conf *config.Config) {
	conf.Room.TokenRevocation = true
}

// connection attempts with a given token must be rejected at the websocket
// handshake
func requireTokenRejected(t *testing.T, token string) {
	conn, err := testclient.NewWebSocketConn(fmt.Sprintf("ws://localhost:%d", defaultServerPort), token, nil)
	if conn != nil {
		_ = conn.Close()
	}
	require.Error(t, err)
}

func requireTokenAccepted(t *testing.T, token string) {
	conn, err := testclient.NewWebSocketConn(fmt.Sprintf("ws://localhost:%d", defaultServerPort), token, nil)
	require.NoError(t, err)
	_ = conn.Close()
}

func waitForRefreshToken(t *testing.T, c *testclient.RTCClient, previous string) string {
	var refreshed string
	require.Eventually(t, func() bool {
		refreshed = c.RefreshToken()
		return refreshed != "" && refreshed != previous
	}, 5*time.Second, 10*time.Millisecond, "no refreshed token received")
	return refreshed
}

func TestTokenRevocationOnRemoveParticipant(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
		return
	}

	_, finish := setupSingleNodeTestWithConfig("TestTokenRevocationOnRemoveParticipant", enableTokenRevocation)
	defer finish()

	identity := "revoked_user"
	token := joinToken(testRoom, identity, nil)
	c1 := createRTCClientWithToken(token, defaultServerPort, testRTCServicePathv0, nil)
	require.NoError(t, c1.WaitUntilConnected(20*time.Second))

	// the server pushes a refreshed token right after join; a kicked client
	// would use it to rejoin
	refreshed := waitForRefreshToken(t, c1, "")

	adminCtx := contextWithToken(adminRoomToken(testRoom))
	_, err := roomClient.RemoveParticipant(adminCtx, &livekit.RoomParticipantIdentity{
		Room:     testRoom,
		Identity: identity,
	})
	require.NoError(t, err)

	// both the original and the refreshed token are now rejected
	requireTokenRejected(t, token)
	requireTokenRejected(t, refreshed)

	// other participants are unaffected
	requireTokenAccepted(t, joinToken(testRoom, "other_user", nil))

	// a token minted after the removal admits the same identity again. the
	// cutoff covers the second of the removal, so step past it
	time.Sleep(1500 * time.Millisecond)
	requireTokenAccepted(t, joinToken(testRoom, identity, nil))

	c1.Stop()
}

func TestTokenRevocationOnUpdateParticipant(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
		return
	}

	_, finish := setupSingleNodeTestWithConfig("TestTokenRevocationOnUpdateParticipant", enableTokenRevocation)
	defer finish()

	identity := "downgraded_user"
	token := joinToken(testRoom, identity, nil)
	c1 := createRTCClientWithToken(token, defaultServerPort, testRTCServicePathv0, nil)
	require.NoError(t, c1.WaitUntilConnected(20*time.Second))

	preUpdate := waitForRefreshToken(t, c1, "")

	// the cutoff on permission updates is floored to the current second so the
	// refreshed token minted right after stays valid; step past the second the
	// tokens under test were minted in, otherwise they'd share it with the cutoff
	time.Sleep(1100 * time.Millisecond)

	adminCtx := contextWithToken(adminRoomToken(testRoom))
	_, err := roomClient.UpdateParticipant(adminCtx, &livekit.UpdateParticipantRequest{
		Room:     testRoom,
		Identity: identity,
		Permission: &livekit.ParticipantPermission{
			CanSubscribe:   true,
			CanPublish:     false,
			CanPublishData: true,
		},
	})
	require.NoError(t, err)

	// tokens carrying the old permissions are rejected
	requireTokenRejected(t, token)
	requireTokenRejected(t, preUpdate)

	// the participant stays connected and receives a refreshed token minted
	// after the cutoff, which is accepted
	postUpdate := waitForRefreshToken(t, c1, preUpdate)
	requireTokenAccepted(t, postUpdate)

	c1.Stop()
}
