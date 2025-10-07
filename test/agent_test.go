// Copyright 2023 LiveKit, Inc.
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

	"github.com/livekit/livekit-server/pkg/testutils"
	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
)

var (
	RegisterTimeout  = 2 * time.Second
	AssignJobTimeout = 3 * time.Second
)

func TestAgents(t *testing.T) {
	for _, useSinglePeerConnection := range []bool{false, true} {
		t.Run(fmt.Sprintf("singlePeerConnection=%+v", useSinglePeerConnection), func(t *testing.T) {
			_, finish := setupSingleNodeTest("TestAgents")
			defer finish()

			ac1, err := newAgentClient(agentToken(), defaultServerPort)
			require.NoError(t, err)
			ac2, err := newAgentClient(agentToken(), defaultServerPort)
			require.NoError(t, err)
			ac3, err := newAgentClient(agentToken(), defaultServerPort)
			require.NoError(t, err)
			ac4, err := newAgentClient(agentToken(), defaultServerPort)
			require.NoError(t, err)
			ac5, err := newAgentClient(agentToken(), defaultServerPort)
			require.NoError(t, err)
			ac6, err := newAgentClient(agentToken(), defaultServerPort)
			require.NoError(t, err)
			defer ac1.close()
			defer ac2.close()
			defer ac3.close()
			defer ac4.close()
			defer ac5.close()
			defer ac6.close()
			ac1.Run(livekit.JobType_JT_ROOM, "default")
			ac2.Run(livekit.JobType_JT_ROOM, "default")
			ac3.Run(livekit.JobType_JT_PUBLISHER, "default")
			ac4.Run(livekit.JobType_JT_PUBLISHER, "default")
			ac5.Run(livekit.JobType_JT_PARTICIPANT, "default")
			ac6.Run(livekit.JobType_JT_PARTICIPANT, "default")

			testutils.WithTimeout(t, func() string {
				if ac1.registered.Load() != 1 || ac2.registered.Load() != 1 || ac3.registered.Load() != 1 || ac4.registered.Load() != 1 || ac5.registered.Load() != 1 || ac6.registered.Load() != 1 {
					return "worker not registered"
				}

				return ""
			}, RegisterTimeout)

			c1 := createRTCClient("c1", defaultServerPort, useSinglePeerConnection, nil)
			c2 := createRTCClient("c2", defaultServerPort, useSinglePeerConnection, nil)
			waitUntilConnected(t, c1, c2)

			// publish 2 tracks
			t1, err := c1.AddStaticTrack("audio/opus", "audio", "micro")
			require.NoError(t, err)
			defer t1.Stop()
			t2, err := c1.AddStaticTrack("video/vp8", "video", "webcam")
			require.NoError(t, err)
			defer t2.Stop()

			testutils.WithTimeout(t, func() string {
				if ac1.roomJobs.Load()+ac2.roomJobs.Load() != 1 {
					return "room job not assigned"
				}

				if ac3.publisherJobs.Load()+ac4.publisherJobs.Load() != 1 {
					return fmt.Sprintf("publisher jobs not assigned, ac3: %d, ac4: %d", ac3.publisherJobs.Load(), ac4.publisherJobs.Load())
				}

				if ac5.participantJobs.Load()+ac6.participantJobs.Load() != 2 {
					return fmt.Sprintf("participant jobs not assigned, ac5: %d, ac6: %d", ac5.participantJobs.Load(), ac6.participantJobs.Load())
				}

				return ""
			}, 6*time.Second)

			// publish 2 tracks
			t3, err := c2.AddStaticTrack("audio/opus", "audio", "micro")
			require.NoError(t, err)
			defer t3.Stop()
			t4, err := c2.AddStaticTrack("video/vp8", "video", "webcam")
			require.NoError(t, err)
			defer t4.Stop()

			testutils.WithTimeout(t, func() string {
				if ac1.roomJobs.Load()+ac2.roomJobs.Load() != 1 {
					return "room job must be assigned 1 time"
				}

				if ac3.publisherJobs.Load()+ac4.publisherJobs.Load() != 2 {
					return "2 publisher jobs must assigned"
				}

				if ac5.participantJobs.Load()+ac6.participantJobs.Load() != 2 {
					return "2 participant jobs must assigned"
				}

				return ""
			}, AssignJobTimeout)
		})
	}
}

func TestAgentNamespaces(t *testing.T) {
	for _, useSinglePeerConnection := range []bool{false, true} {
		t.Run(fmt.Sprintf("singlePeerConnection=%+v", useSinglePeerConnection), func(t *testing.T) {
			_, finish := setupSingleNodeTest("TestAgentNamespaces")
			defer finish()

			ac1, err := newAgentClient(agentToken(), defaultServerPort)
			require.NoError(t, err)
			ac2, err := newAgentClient(agentToken(), defaultServerPort)
			require.NoError(t, err)
			defer ac1.close()
			defer ac2.close()
			ac1.Run(livekit.JobType_JT_ROOM, "namespace1")
			ac2.Run(livekit.JobType_JT_ROOM, "namespace2")

			_, err = roomClient.CreateRoom(contextWithToken(createRoomToken()), &livekit.CreateRoomRequest{
				Name: testRoom,
				Agents: []*livekit.RoomAgentDispatch{
					{},
					{
						AgentName: "ag",
					},
				},
			})
			require.NoError(t, err)

			testutils.WithTimeout(t, func() string {
				if ac1.registered.Load() != 1 || ac2.registered.Load() != 1 {
					return "worker not registered"
				}
				return ""
			}, RegisterTimeout)

			c1 := createRTCClient("c1", defaultServerPort, useSinglePeerConnection, nil)
			waitUntilConnected(t, c1)

			testutils.WithTimeout(t, func() string {
				if ac1.roomJobs.Load() != 1 || ac2.roomJobs.Load() != 1 {
					return "room job not assigned"
				}

				job1 := <-ac1.requestedJobs
				job2 := <-ac2.requestedJobs

				if job1.Namespace != "namespace1" {
					return "namespace is not 'namespace'"
				}

				if job2.Namespace != "namespace2" {
					return "namespace is not 'namespace2'"
				}

				if job1.Id == job2.Id {
					return "job ids are the same"
				}

				return ""
			}, AssignJobTimeout)
		})
	}
}

func TestAgentMultiNode(t *testing.T) {
	for _, useSinglePeerConnection := range []bool{false, true} {
		t.Run(fmt.Sprintf("singlePeerConnection=%+v", useSinglePeerConnection), func(t *testing.T) {
			_, _, finish := setupMultiNodeTest("TestAgentMultiNode")
			defer finish()

			ac1, err := newAgentClient(agentToken(), defaultServerPort)
			require.NoError(t, err)
			ac2, err := newAgentClient(agentToken(), defaultServerPort)
			require.NoError(t, err)
			defer ac1.close()
			defer ac2.close()
			ac1.Run(livekit.JobType_JT_ROOM, "default")
			ac2.Run(livekit.JobType_JT_PUBLISHER, "default")

			testutils.WithTimeout(t, func() string {
				if ac1.registered.Load() != 1 || ac2.registered.Load() != 1 {
					return "worker not registered"
				}
				return ""
			}, RegisterTimeout)

			c1 := createRTCClient("c1", secondServerPort, useSinglePeerConnection, nil) // Create a room on the second node
			waitUntilConnected(t, c1)

			t1, err := c1.AddStaticTrack("audio/opus", "audio", "micro")
			require.NoError(t, err)
			defer t1.Stop()

			time.Sleep(time.Second * 10)

			testutils.WithTimeout(t, func() string {
				if ac1.roomJobs.Load() != 1 {
					return "room job not assigned"
				}

				if ac2.publisherJobs.Load() != 1 {
					return "participant job not assigned"
				}

				return ""
			}, AssignJobTimeout)
		})
	}
}

func agentToken() string {
	at := auth.NewAccessToken(testApiKey, testApiSecret).
		AddGrant(&auth.VideoGrant{Agent: true})
	t, err := at.ToJWT()
	if err != nil {
		panic(err)
	}
	return t
}
