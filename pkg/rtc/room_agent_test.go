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

package rtc

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/protocol/utils/guid"

	"github.com/livekit/livekit-server/pkg/agent"
	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/sfu"
	"github.com/livekit/livekit-server/pkg/telemetry"
	"github.com/livekit/livekit-server/pkg/telemetry/telemetryfakes"
	sutils "github.com/livekit/livekit-server/pkg/utils"
)

type recordingAgentClient struct {
	mu       sync.Mutex
	requests []*agent.JobRequest
}

func (c *recordingAgentClient) LaunchJob(_ context.Context, desc *agent.JobRequest) *sutils.IncrementalDispatcher[*livekit.Job] {
	c.mu.Lock()
	c.requests = append(c.requests, desc)
	c.mu.Unlock()
	inc := sutils.NewIncrementalDispatcher[*livekit.Job]()
	inc.Done()
	return inc
}

func (c *recordingAgentClient) TerminateJob(context.Context, string, rpc.JobTerminateReason) (*livekit.JobState, error) {
	return nil, nil
}

func (c *recordingAgentClient) Stop() error { return nil }

func (c *recordingAgentClient) requestFor(agentName string) *agent.JobRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range c.requests {
		if r.AgentName == agentName {
			return r
		}
	}
	return nil
}

func newRoomWithAgentClient(t *testing.T, internal *livekit.RoomInternal, client agent.Client) *Room {
	rm := NewRoom(
		&livekit.Room{Name: "room"},
		internal,
		WebRTCConfig{},
		config.RoomConfig{EmptyTimeout: 5 * 60, DepartureTimeout: 1},
		&sfu.AudioConfig{},
		&livekit.ServerInfo{NodeId: "testnode"},
		telemetry.NewTelemetryService(nil, &telemetryfakes.FakeAnalyticsService{}),
		client, nil, nil,
	)
	t.Cleanup(func() { rm.Close(types.RoomCloseReasonUnknown) })
	return rm
}

func TestAgentDispatchThreadID(t *testing.T) {
	t.Run("a room configuration dispatch launches its job on the dispatch's thread", func(t *testing.T) {
		threadID := guid.New(guid.AgentThreadPrefix)
		client := &recordingAgentClient{}
		rm := newRoomWithAgentClient(t, &livekit.RoomInternal{
			AgentDispatches: []*livekit.RoomAgentDispatch{{AgentName: "front-desk", ThreadId: threadID}},
		}, client)

		dispatches, err := rm.GetAgentDispatches("")
		require.NoError(t, err)
		require.Len(t, dispatches, 1)
		require.Equal(t, threadID, dispatches[0].ThreadId)

		require.Eventually(t, func() bool { return client.requestFor("front-desk") != nil }, time.Second, 10*time.Millisecond)
		require.Equal(t, threadID, client.requestFor("front-desk").ThreadId)
	})

	t.Run("an added dispatch launches its job on the dispatch's thread", func(t *testing.T) {
		threadID := guid.New(guid.AgentThreadPrefix)
		client := &recordingAgentClient{}
		rm := newRoomWithAgentClient(t, nil, client)

		_, err := rm.AddAgentDispatch(&livekit.AgentDispatch{Id: "AD_added", AgentName: "back-office", ThreadId: threadID})
		require.NoError(t, err)

		require.Eventually(t, func() bool { return client.requestFor("back-office") != nil }, time.Second, 10*time.Millisecond)
		require.Equal(t, threadID, client.requestFor("back-office").ThreadId)
	})
}
