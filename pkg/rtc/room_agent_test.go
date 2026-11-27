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
	"errors"
	"fmt"
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

// agentStoreWithHook calls onStoreDispatch while a dispatch is being stored and returns storeErr
type agentStoreWithHook struct {
	onStoreDispatch func()
	storeErr        error
}

func (s *agentStoreWithHook) StoreAgentDispatch(context.Context, *livekit.AgentDispatch) error {
	if s.onStoreDispatch != nil {
		s.onStoreDispatch()
	}
	return s.storeErr
}

func (s *agentStoreWithHook) DeleteAgentDispatch(context.Context, *livekit.AgentDispatch) error {
	return nil
}

func (s *agentStoreWithHook) ListAgentDispatches(context.Context, livekit.RoomName) ([]*livekit.AgentDispatch, error) {
	return nil, nil
}

func (s *agentStoreWithHook) StoreAgentJob(context.Context, *livekit.Job) error { return nil }

func (s *agentStoreWithHook) DeleteAgentJob(context.Context, *livekit.Job) error { return nil }

func (c *recordingAgentClient) countRequests(agentName string, jobType livekit.JobType, identity livekit.ParticipantIdentity) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, r := range c.requests {
		if r.AgentName == agentName && r.JobType == jobType && r.Participant.GetIdentity() == string(identity) {
			n++
		}
	}
	return n
}

// waitForDispatchLaunches waits until every job launch of the dispatch has called LaunchJob
func waitForDispatchLaunches(t *testing.T, rm *Room, dispatchID string) {
	rm.lock.RLock()
	ad := rm.agentDispatches[dispatchID]
	rm.lock.RUnlock()
	require.NotNil(t, ad)
	ad.waitForPendingJobs()
}

func TestAddAgentDispatchLaunchesTargetJobsOnce(t *testing.T) {
	newRoom := func(t *testing.T, client agent.Client, store AgentStore) *Room {
		rm := NewRoom(
			&livekit.Room{Name: "room"},
			nil,
			WebRTCConfig{},
			config.RoomConfig{EmptyTimeout: 5 * 60, DepartureTimeout: 1},
			&sfu.AudioConfig{},
			&livekit.ServerInfo{NodeId: "testnode"},
			telemetry.NewTelemetryService(nil, &telemetryfakes.FakeAnalyticsService{}),
			client, store, nil,
		)
		t.Cleanup(func() { rm.Close(types.RoomCloseReasonUnknown) })
		return rm
	}

	t.Run("a participant joining while the dispatch is stored gets one participant job", func(t *testing.T) {
		client := &recordingAgentClient{}
		store := &agentStoreWithHook{}
		rm := newRoom(t, client, store)

		p := NewMockParticipant("joiner", types.CurrentProtocol, false, false, rm.LocalParticipantListener())
		store.onStoreDispatch = func() {
			require.NoError(t, rm.Join(p, nil, nil, nil))
		}

		_, err := rm.AddAgentDispatch(&livekit.AgentDispatch{Id: "AD_join", AgentName: "per-participant", Room: "room"})
		require.NoError(t, err)

		waitForDispatchLaunches(t, rm, "AD_join")
		require.Equal(t, 1, client.countRequests("per-participant", livekit.JobType_JT_PARTICIPANT, "joiner"))
	})

	t.Run("a dispatch added before the room handles the first track publish gets one publisher job", func(t *testing.T) {
		client := &recordingAgentClient{}
		rm := newRoom(t, client, &agentStoreWithHook{})

		p := NewMockParticipant("pub", types.CurrentProtocol, false, false, rm.LocalParticipantListener())
		require.NoError(t, rm.Join(p, nil, nil, nil))

		// ParticipantImpl marks itself a publisher before the room's OnTrackPublished runs
		p.IsPublisherReturns(true)
		_, err := rm.AddAgentDispatch(&livekit.AgentDispatch{Id: "AD_publish", AgentName: "per-publisher", Room: "room"})
		require.NoError(t, err)
		waitForDispatchLaunches(t, rm, "AD_publish")
		require.Equal(t, 0, client.countRequests("per-publisher", livekit.JobType_JT_PUBLISHER, "pub"))

		rm.LocalParticipantListener().OnTrackPublished(p, NewMockTrack(livekit.TrackType_AUDIO, "mic"))
		waitForDispatchLaunches(t, rm, "AD_publish")
		require.Equal(t, 1, client.countRequests("per-publisher", livekit.JobType_JT_PUBLISHER, "pub"))

		// later tracks do not launch again
		rm.LocalParticipantListener().OnTrackPublished(p, NewMockTrack(livekit.TrackType_VIDEO, "cam"))
		waitForDispatchLaunches(t, rm, "AD_publish")
		require.Equal(t, 1, client.countRequests("per-publisher", livekit.JobType_JT_PUBLISHER, "pub"))
	})

	t.Run("a dispatch added after the first track publish gets one publisher job", func(t *testing.T) {
		client := &recordingAgentClient{}
		rm := newRoom(t, client, &agentStoreWithHook{})

		p := NewMockParticipant("pub", types.CurrentProtocol, false, false, rm.LocalParticipantListener())
		require.NoError(t, rm.Join(p, nil, nil, nil))
		p.IsPublisherReturns(true)
		rm.LocalParticipantListener().OnTrackPublished(p, NewMockTrack(livekit.TrackType_AUDIO, "mic"))

		_, err := rm.AddAgentDispatch(&livekit.AgentDispatch{Id: "AD_published", AgentName: "per-publisher", Room: "room"})
		require.NoError(t, err)

		waitForDispatchLaunches(t, rm, "AD_published")
		require.Equal(t, 1, client.countRequests("per-publisher", livekit.JobType_JT_PUBLISHER, "pub"))
		require.Equal(t, 1, client.countRequests("per-publisher", livekit.JobType_JT_PARTICIPANT, "pub"))
	})

	t.Run("a participant that unpublished all tracks gets no publisher job", func(t *testing.T) {
		client := &recordingAgentClient{}
		rm := newRoom(t, client, &agentStoreWithHook{})

		p := NewMockParticipant("pub", types.CurrentProtocol, false, false, rm.LocalParticipantListener())
		require.NoError(t, rm.Join(p, nil, nil, nil))
		p.IsPublisherReturns(true)
		rm.LocalParticipantListener().OnTrackPublished(p, NewMockTrack(livekit.TrackType_AUDIO, "mic"))
		p.IsPublisherReturns(false)

		_, err := rm.AddAgentDispatch(&livekit.AgentDispatch{Id: "AD_unpublished", AgentName: "per-publisher", Room: "room"})
		require.NoError(t, err)

		waitForDispatchLaunches(t, rm, "AD_unpublished")
		require.Equal(t, 0, client.countRequests("per-publisher", livekit.JobType_JT_PUBLISHER, "pub"))
		require.Equal(t, 1, client.countRequests("per-publisher", livekit.JobType_JT_PARTICIPANT, "pub"))
	})

	t.Run("a dispatch that cannot be stored is not added or launched", func(t *testing.T) {
		client := &recordingAgentClient{}
		rm := newRoom(t, client, &agentStoreWithHook{storeErr: errors.New("store failed")})

		p := NewMockParticipant("joiner", types.CurrentProtocol, false, false, rm.LocalParticipantListener())
		require.NoError(t, rm.Join(p, nil, nil, nil))

		_, err := rm.AddAgentDispatch(&livekit.AgentDispatch{Id: "AD_unstored", AgentName: "unstored", Room: "room"})
		require.Error(t, err)

		dispatches, err := rm.GetAgentDispatches("AD_unstored")
		require.NoError(t, err)
		require.Empty(t, dispatches)

		// a later join does not pick it up either
		p2 := NewMockParticipant("later", types.CurrentProtocol, false, false, rm.LocalParticipantListener())
		require.NoError(t, rm.Join(p2, nil, nil, nil))

		client.mu.Lock()
		defer client.mu.Unlock()
		for _, r := range client.requests {
			require.NotEqual(t, "unstored", r.AgentName)
		}
	})

	t.Run("participants joining concurrently with added dispatches get each job once", func(t *testing.T) {
		client := &recordingAgentClient{}
		// storing takes about as long as a Redis write
		rm := newRoom(t, client, &agentStoreWithHook{onStoreDispatch: func() { time.Sleep(time.Millisecond) }})

		const numParticipants = 20
		const numDispatches = 5
		var wg sync.WaitGroup
		for i := range numParticipants {
			wg.Add(1)
			go func() {
				defer wg.Done()
				identity := livekit.ParticipantIdentity(fmt.Sprintf("p%d", i))
				p := NewMockParticipant(identity, types.CurrentProtocol, false, false, rm.LocalParticipantListener())
				require.NoError(t, rm.Join(p, nil, nil, nil))
			}()
		}
		for i := range numDispatches {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := rm.AddAgentDispatch(&livekit.AgentDispatch{
					Id:        fmt.Sprintf("AD_%d", i),
					AgentName: fmt.Sprintf("agent%d", i),
					Room:      "room",
				})
				require.NoError(t, err)
			}()
		}
		wg.Wait()

		for i := range numDispatches {
			waitForDispatchLaunches(t, rm, fmt.Sprintf("AD_%d", i))
		}
		for i := range numDispatches {
			for j := range numParticipants {
				identity := livekit.ParticipantIdentity(fmt.Sprintf("p%d", j))
				require.Equal(t, 1, client.countRequests(fmt.Sprintf("agent%d", i), livekit.JobType_JT_PARTICIPANT, identity),
					"agent%d, %s", i, identity)
			}
		}
	})
}
