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
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
)

func TestAgents(t *testing.T) {
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
	defer ac1.close()
	defer ac2.close()
	defer ac3.close()
	defer ac4.close()
	ac1.Run(livekit.JobType_JT_ROOM, "namespace")
	ac2.Run(livekit.JobType_JT_ROOM, "namespace")
	ac3.Run(livekit.JobType_JT_PUBLISHER, "namespace")
	ac4.Run(livekit.JobType_JT_PUBLISHER, "namespace")

	time.Sleep(time.Second * 3)

	require.Equal(t, int32(1), ac1.registered.Load())
	require.Equal(t, int32(1), ac2.registered.Load())
	require.Equal(t, int32(1), ac3.registered.Load())
	require.Equal(t, int32(1), ac4.registered.Load())

	c1 := createRTCClient("c1", defaultServerPort, nil)
	c2 := createRTCClient("c2", defaultServerPort, nil)
	waitUntilConnected(t, c1, c2)

	// publish 2 tracks
	t1, err := c1.AddStaticTrack("audio/opus", "audio", "micro")
	require.NoError(t, err)
	defer t1.Stop()
	t2, err := c1.AddStaticTrack("video/vp8", "video", "webcam")
	require.NoError(t, err)
	defer t2.Stop()

	time.Sleep(time.Second * 10)

	require.Equal(t, int32(1), ac1.roomJobs.Load()+ac2.roomJobs.Load())
	require.Equal(t, int32(1), ac3.participantJobs.Load()+ac4.participantJobs.Load())

	// publish 2 tracks
	t3, err := c2.AddStaticTrack("audio/opus", "audio", "micro")
	require.NoError(t, err)
	defer t3.Stop()
	t4, err := c2.AddStaticTrack("video/vp8", "video", "webcam")
	require.NoError(t, err)
	defer t4.Stop()

	time.Sleep(time.Second * 3)

	require.Equal(t, int32(1), ac1.roomJobs.Load()+ac2.roomJobs.Load())
	require.Equal(t, int32(2), ac3.participantJobs.Load()+ac4.participantJobs.Load())
}

func TestAgentNamespaces(t *testing.T) {
	_, finish := setupSingleNodeTest("TestAgentNamespaces")
	defer finish()

	ac1, err := newAgentClient(agentToken(), defaultServerPort)
	require.NoError(t, err)
	ac2, err := newAgentClient(agentToken(), defaultServerPort)
	require.NoError(t, err)
	defer ac1.close()
	defer ac2.close()
	ac1.Run(livekit.JobType_JT_ROOM, "namespace")
	ac2.Run(livekit.JobType_JT_ROOM, "namespace2")

	time.Sleep(time.Second * 3)

	require.Equal(t, int32(1), ac1.registered.Load())
	require.Equal(t, int32(1), ac2.registered.Load())

	c1 := createRTCClient("c1", defaultServerPort, nil)
	waitUntilConnected(t, c1)

	time.Sleep(time.Second * 6)

	require.Equal(t, int32(1), ac1.roomJobs.Load())
	require.Equal(t, int32(1), ac2.roomJobs.Load())

	job1 := <-ac1.requestedJobs
	job2 := <-ac2.requestedJobs

	require.Equal(t, "namespace", job1.Namespace)
	require.Equal(t, "namespace2", job2.Namespace)
	require.NotEqual(t, job1.Id, job2.Id)
}

func TestAgentMultiNode(t *testing.T) {
	_, _, finish := setupMultiNodeTest("TestAgentMultiNode")
	defer finish()

	ac1, err := newAgentClient(agentToken(), defaultServerPort)
	require.NoError(t, err)
	ac2, err := newAgentClient(agentToken(), defaultServerPort) 
	defer ac1.close()
	defer ac2.close()
	ac1.Run(livekit.JobType_JT_ROOM, "namespace")
	ac2.Run(livekit.JobType_JT_PUBLISHER, "namespace")

	time.Sleep(time.Second * 3)

	require.Equal(t, int32(1), ac1.registered.Load())
	require.Equal(t, int32(1), ac2.registered.Load())

	c1 := createRTCClient("c1", secondServerPort, nil) // Create a room on the second node
	waitUntilConnected(t, c1)

	t1, err := c1.AddStaticTrack("audio/opus", "audio", "micro")
	require.NoError(t, err)
	defer t1.Stop()

	time.Sleep(time.Second * 10)

	require.Equal(t, int32(1), ac1.roomJobs.Load())
	require.Equal(t, int32(1), ac2.participantJobs.Load())

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
