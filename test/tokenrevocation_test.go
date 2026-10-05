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

	testclient "github.com/livekit/livekit-server/test/client"
)

func TestSingleNodeTokenRevocation(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
		return
	}

	_, finish := setupSingleNodeTest("TestSingleNodeTokenRevocation")
	defer finish()

	for _, testRTCServicePath := range testRTCServicePaths {
		t.Run(fmt.Sprintf("testRTCServicePath=%s", testRTCServicePath.String()), func(t *testing.T) {
			suffix := testRTCServicePath.String()
			ctx := contextWithToken(adminRoomToken(testRoom))

			requireRejected := func(t *testing.T, token string) {
				opts := &testclient.Options{AutoSubscribe: true}
				testRTCServicePathToTestClientOptions(testRTCServicePath, opts)
				_, err := testclient.NewWebSocketConn(fmt.Sprintf("ws://localhost:%d", defaultServerPort), token, opts)
				require.Error(t, err)
			}

			connect := func(t *testing.T, token string) {
				c := createRTCClientWithToken(token, defaultServerPort, testRTCServicePath, nil)
				t.Cleanup(c.Stop)
				waitUntilConnected(t, c)
			}

			// Review: "Failed removals leave revocations behind".
			t.Run("failed removal keeps tokens valid", func(t *testing.T) {
				identity := "absent_" + suffix
				token := joinToken(testRoom, identity, nil)

				_, err := roomClient.RemoveParticipant(ctx, &livekit.RoomParticipantIdentity{
					Room:     testRoom,
					Identity: identity,
				})
				require.Error(t, err)

				connect(t, token)
			})

			t.Run("default revocation includes the leeway", func(t *testing.T) {
				identity := "removed_" + suffix
				token := joinToken(testRoom, identity, nil)
				connect(t, token)

				_, err := roomClient.RemoveParticipant(ctx, &livekit.RoomParticipantIdentity{
					Room:     testRoom,
					Identity: identity,
				})
				require.NoError(t, err)

				requireRejected(t, token)
				// a token issued within the leeway after the removal is revoked too
				requireRejected(t, joinToken(testRoom, identity, nil))
			})

			t.Run("explicit revoke_token_ts", func(t *testing.T) {
				identity := "removed_explicit_" + suffix
				token := joinToken(testRoom, identity, nil)
				connect(t, token)

				revokeTokenTs := time.Now().Add(time.Second).Unix()
				_, err := roomClient.RemoveParticipant(ctx, &livekit.RoomParticipantIdentity{
					Room:          testRoom,
					Identity:      identity,
					RevokeTokenTs: revokeTokenTs,
				})
				require.NoError(t, err)

				requireRejected(t, token)

				// a token issued from revoke_token_ts on is accepted
				time.Sleep(time.Until(time.Unix(revokeTokenTs, 0)) + 100*time.Millisecond)
				connect(t, joinToken(testRoom, identity, nil))
			})
		})
	}
}
