// Copyright 2024 LiveKit, Inc.
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

// These integration tests verify that ListParticipants, GetParticipant,
// RemoveParticipant, and UpdateParticipant return consistent data immediately
// after a participant connects — without the eventual consistency gap that
// occurred when these APIs read from Redis instead of the authoritative
// in-memory room state.

package test

import (
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils/guid"
	"github.com/livekit/protocol/webhook"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/service"
	"github.com/livekit/livekit-server/pkg/testutils"
)

// =============================================================================
// Single-node tests
// =============================================================================

// TestSingleNodeListParticipantsImmediateConsistency verifies that a participant
// appears in ListParticipants immediately after connecting.
func TestSingleNodeListParticipantsImmediateConsistency(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
		return
	}

	_, finish := setupSingleNodeTest("TestSingleNodeListParticipantsImmediateConsistency")
	defer finish()

	for _, testRTCServicePath := range testRTCServicePaths {
		t.Run(fmt.Sprintf("testRTCServicePath=%s", testRTCServicePath.String()), func(t *testing.T) {
			c1 := createRTCClient("list_consistency_p1", defaultServerPort, testRTCServicePath, nil)
			defer c1.Stop()
			waitUntilConnected(t, c1)

			ctx := contextWithToken(adminRoomToken(testRoom))

			testutils.WithTimeout(t, func() string {
				res, err := roomClient.ListParticipants(ctx, &livekit.ListParticipantsRequest{
					Room: testRoom,
				})
				if err != nil {
					return fmt.Sprintf("ListParticipants error: %v", err)
				}
				for _, p := range res.Participants {
					if p.Identity == "list_consistency_p1" {
						return ""
					}
				}
				return fmt.Sprintf("participant not found in list, got %d participants", len(res.Participants))
			})
		})
	}
}

// TestSingleNodeGetParticipantImmediateConsistency verifies that GetParticipant
// returns the participant immediately after connecting.
func TestSingleNodeGetParticipantImmediateConsistency(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
		return
	}

	_, finish := setupSingleNodeTest("TestSingleNodeGetParticipantImmediateConsistency")
	defer finish()

	for _, testRTCServicePath := range testRTCServicePaths {
		t.Run(fmt.Sprintf("testRTCServicePath=%s", testRTCServicePath.String()), func(t *testing.T) {
			c1 := createRTCClient("get_consistency_p1", defaultServerPort, testRTCServicePath, nil)
			defer c1.Stop()
			waitUntilConnected(t, c1)

			ctx := contextWithToken(adminRoomToken(testRoom))

			testutils.WithTimeout(t, func() string {
				p, err := roomClient.GetParticipant(ctx, &livekit.RoomParticipantIdentity{
					Room:     testRoom,
					Identity: "get_consistency_p1",
				})
				if err != nil {
					return fmt.Sprintf("GetParticipant error: %v", err)
				}
				if p.Identity != "get_consistency_p1" {
					return fmt.Sprintf("wrong identity: %s", p.Identity)
				}
				return ""
			})
		})
	}
}

// TestSingleNodeMultipleParticipantsConsistency verifies that when multiple
// participants join in quick succession, ListParticipants returns all of them.
func TestSingleNodeMultipleParticipantsConsistency(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
		return
	}

	_, finish := setupSingleNodeTest("TestSingleNodeMultipleParticipantsConsistency")
	defer finish()

	for _, testRTCServicePath := range testRTCServicePaths {
		t.Run(fmt.Sprintf("testRTCServicePath=%s", testRTCServicePath.String()), func(t *testing.T) {
			c1 := createRTCClient("multi_p1", defaultServerPort, testRTCServicePath, nil)
			c2 := createRTCClient("multi_p2", defaultServerPort, testRTCServicePath, nil)
			c3 := createRTCClient("multi_p3", defaultServerPort, testRTCServicePath, nil)
			defer stopClients(c1, c2, c3)
			waitUntilConnected(t, c1, c2, c3)

			ctx := contextWithToken(adminRoomToken(testRoom))

			testutils.WithTimeout(t, func() string {
				res, err := roomClient.ListParticipants(ctx, &livekit.ListParticipantsRequest{
					Room: testRoom,
				})
				if err != nil {
					return fmt.Sprintf("ListParticipants error: %v", err)
				}
				if len(res.Participants) != 3 {
					return fmt.Sprintf("expected 3 participants, got %d", len(res.Participants))
				}
				return ""
			})

			for _, identity := range []string{"multi_p1", "multi_p2", "multi_p3"} {
				p, err := roomClient.GetParticipant(ctx, &livekit.RoomParticipantIdentity{
					Room:     testRoom,
					Identity: identity,
				})
				require.NoError(t, err)
				require.Equal(t, identity, p.Identity)
			}
		})
	}
}

// TestSingleNodeUpdateParticipantImmediatelyAfterJoin verifies that
// UpdateParticipant works immediately after a participant joins, without
// being blocked by a stale Redis guard.
func TestSingleNodeUpdateParticipantImmediatelyAfterJoin(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
		return
	}

	_, finish := setupSingleNodeTest("TestSingleNodeUpdateParticipantImmediatelyAfterJoin")
	defer finish()

	for _, testRTCServicePath := range testRTCServicePaths {
		t.Run(fmt.Sprintf("testRTCServicePath=%s", testRTCServicePath.String()), func(t *testing.T) {
			c1 := createRTCClient("update_imm_p1", defaultServerPort, testRTCServicePath, nil)
			defer c1.Stop()
			waitUntilConnected(t, c1)

			ctx := contextWithToken(adminRoomToken(testRoom))

			testutils.WithTimeout(t, func() string {
				res, err := roomClient.UpdateParticipant(ctx, &livekit.UpdateParticipantRequest{
					Room:     testRoom,
					Identity: "update_imm_p1",
					Metadata: "immediate-metadata",
				})
				if err != nil {
					return fmt.Sprintf("UpdateParticipant error: %v", err)
				}
				if res.Metadata != "immediate-metadata" {
					return fmt.Sprintf("metadata not updated: %s", res.Metadata)
				}
				return ""
			})
		})
	}
}

// =============================================================================
// Multi-node tests
// =============================================================================

// TestMultiNodeListParticipantsConsistency verifies that ListParticipants returns
// accurate data in a multi-node setup where the API call may hit a different node
// than the one hosting the room.
func TestMultiNodeListParticipantsConsistency(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
		return
	}

	for _, testRTCServicePath := range testRTCServicePaths {
		t.Run(fmt.Sprintf("testRTCServicePath=%s", testRTCServicePath.String()), func(t *testing.T) {
			_, _, finish := setupMultiNodeTest("TestMultiNodeListParticipantsConsistency", t)
			defer finish()

			c1 := createRTCClient("mn_list_p1", defaultServerPort, testRTCServicePath, nil)
			defer c1.Stop()
			waitUntilConnected(t, c1)

			ctx := contextWithToken(adminRoomToken(testRoom))

			testutils.WithTimeout(t, func() string {
				res, err := roomClient.ListParticipants(ctx, &livekit.ListParticipantsRequest{
					Room: testRoom,
				})
				if err != nil {
					return fmt.Sprintf("ListParticipants error: %v", err)
				}
				for _, p := range res.Participants {
					if p.Identity == "mn_list_p1" {
						return ""
					}
				}
				return fmt.Sprintf("participant not found, got %d participants", len(res.Participants))
			})
		})
	}
}

// TestMultiNodeGetParticipantConsistency verifies GetParticipant in multi-node.
func TestMultiNodeGetParticipantConsistency(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
		return
	}

	for _, testRTCServicePath := range testRTCServicePaths {
		t.Run(fmt.Sprintf("testRTCServicePath=%s", testRTCServicePath.String()), func(t *testing.T) {
			_, _, finish := setupMultiNodeTest("TestMultiNodeGetParticipantConsistency", t)
			defer finish()

			c1 := createRTCClient("mn_get_p1", defaultServerPort, testRTCServicePath, nil)
			defer c1.Stop()
			waitUntilConnected(t, c1)

			ctx := contextWithToken(adminRoomToken(testRoom))

			testutils.WithTimeout(t, func() string {
				p, err := roomClient.GetParticipant(ctx, &livekit.RoomParticipantIdentity{
					Room:     testRoom,
					Identity: "mn_get_p1",
				})
				if err != nil {
					return fmt.Sprintf("GetParticipant error: %v", err)
				}
				if p.Identity != "mn_get_p1" {
					return fmt.Sprintf("wrong identity: %s", p.Identity)
				}
				return ""
			})
		})
	}
}

// TestMultiNodeUpdateParticipantConsistency verifies UpdateParticipant works
// immediately in multi-node without the Redis guard blocking it.
func TestMultiNodeUpdateParticipantConsistency(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
		return
	}

	for _, testRTCServicePath := range testRTCServicePaths {
		t.Run(fmt.Sprintf("testRTCServicePath=%s", testRTCServicePath.String()), func(t *testing.T) {
			_, _, finish := setupMultiNodeTest("TestMultiNodeUpdateParticipantConsistency", t)
			defer finish()

			c1 := createRTCClient("mn_update_p1", defaultServerPort, testRTCServicePath, nil)
			defer c1.Stop()
			waitUntilConnected(t, c1)

			ctx := contextWithToken(adminRoomToken(testRoom))

			testutils.WithTimeout(t, func() string {
				res, err := roomClient.UpdateParticipant(ctx, &livekit.UpdateParticipantRequest{
					Room:     testRoom,
					Identity: "mn_update_p1",
					Metadata: "updated-immediately",
				})
				if err != nil {
					return fmt.Sprintf("UpdateParticipant error: %v", err)
				}
				if res.Metadata != "updated-immediately" {
					return fmt.Sprintf("metadata not updated: %s", res.Metadata)
				}
				return ""
			})
		})
	}
}

// =============================================================================
// Webhook race condition reproduction test
// =============================================================================

// TestListParticipantsConsistentAfterWebhook reproduces the original bug:
// participant_joined webhook fires, backend immediately calls ListParticipants,
// participant is missing from the response.
func TestListParticipantsConsistentAfterWebhook(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
		return
	}

	for _, testRTCServicePath := range testRTCServicePaths {
		t.Run(fmt.Sprintf("testRTCServicePath=%s", testRTCServicePath.String()), func(t *testing.T) {
			server, ts, finish, err := setupServerWithWebhookAndListCheck()
			require.NoError(t, err)
			defer finish()

			_ = server

			c1 := createRTCClient("webhook_race_p1", defaultServerPort, testRTCServicePath, nil)
			defer c1.Stop()
			waitUntilConnected(t, c1)

			testutils.WithTimeout(t, func() string {
				if ts.GetEvent(webhook.EventParticipantJoined) == nil {
					return "did not receive ParticipantJoined webhook"
				}
				return ""
			})

			listErr := ts.GetListParticipantsError()
			require.Empty(t, listErr, "ListParticipants called from webhook handler failed: %s", listErr)

			joined := ts.GetEvent(webhook.EventParticipantJoined)
			require.Equal(t, "webhook_race_p1", joined.Participant.Identity)
		})
	}
}

// --- Webhook test server that calls ListParticipants on participant_joined ---

type webhookListCheckServer struct {
	webhookTestServer
	listCheckError string
	listCheckMu    sync.Mutex
}

func newWebhookListCheckServer(addr string) *webhookListCheckServer {
	s := &webhookListCheckServer{}
	s.events = make(map[string]*livekit.WebhookEvent)
	s.provider = auth.NewFileBasedKeyProviderFromMap(map[string]string{testApiKey: testApiSecret})
	s.server = &http.Server{
		Addr:    addr,
		Handler: s,
	}
	return s
}

func (s *webhookListCheckServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	data, err := webhook.Receive(r, s.provider)
	if err != nil {
		logger.Errorw("could not receive webhook", err)
		return
	}

	event := livekit.WebhookEvent{}
	if err = protojson.Unmarshal(data, &event); err != nil {
		logger.Errorw("could not unmarshal event", err)
		return
	}

	s.lock.Lock()
	s.events[event.Event] = &event
	s.lock.Unlock()

	if event.Event == webhook.EventParticipantJoined && event.Room != nil {
		listClient := livekit.NewRoomServiceJSONClient(
			fmt.Sprintf("http://localhost:%d", defaultServerPort),
			&http.Client{},
		)

		ctx := contextWithToken(adminRoomToken(event.Room.Name))
		res, err := listClient.ListParticipants(ctx, &livekit.ListParticipantsRequest{
			Room: event.Room.Name,
		})

		s.listCheckMu.Lock()
		defer s.listCheckMu.Unlock()

		if err != nil {
			s.listCheckError = fmt.Sprintf("ListParticipants error: %v", err)
			return
		}

		found := false
		for _, p := range res.Participants {
			if p.Identity == event.Participant.Identity {
				found = true
				break
			}
		}
		if !found {
			identities := make([]string, 0, len(res.Participants))
			for _, p := range res.Participants {
				identities = append(identities, p.Identity)
			}
			s.listCheckError = fmt.Sprintf(
				"participant %q from webhook not found in ListParticipants (got %v)",
				event.Participant.Identity, identities,
			)
		}
	}
}

func (s *webhookListCheckServer) GetListParticipantsError() string {
	s.listCheckMu.Lock()
	defer s.listCheckMu.Unlock()
	return s.listCheckError
}

func setupServerWithWebhookAndListCheck() (server *service.LivekitServer, testServer *webhookListCheckServer, finishFunc func(), err error) {
	conf, err := config.NewConfig("", true, nil, nil)
	if err != nil {
		panic(fmt.Sprintf("could not create config: %v", err))
	}
	conf.WebHook.URLs = []string{"http://localhost:7891"}
	conf.WebHook.APIKey = testApiKey
	conf.Keys = map[string]string{testApiKey: testApiSecret}

	testServer = newWebhookListCheckServer(":7891")
	if err = testServer.Start(); err != nil {
		return
	}

	currentNode, err := routing.NewLocalNode(conf)
	if err != nil {
		return
	}
	currentNode.SetNodeID(livekit.NodeID(guid.New(nodeID1)))

	server, err = service.InitializeServer(conf, currentNode)
	if err != nil {
		return
	}

	go func() {
		if err := server.Start(); err != nil {
			logger.Errorw("server returned error", err)
		}
	}()

	waitForServerToStart(server)

	roomClient = livekit.NewRoomServiceJSONClient(
		fmt.Sprintf("http://localhost:%d", defaultServerPort),
		&http.Client{},
	)

	finishFunc = func() {
		server.Stop(true)
		testServer.Stop()
	}
	return
}
