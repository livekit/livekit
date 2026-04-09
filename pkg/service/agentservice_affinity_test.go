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

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/livekit/livekit-server/pkg/agent"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
)

// stubAgentInternalServer satisfies rpc.AgentInternalServer for unit tests that
// only exercise AgentHandler logic and never call into the RPC layer.
type stubAgentInternalServer struct{}

func (s *stubAgentInternalServer) RegisterJobRequestTopic(_, _ string) error             { return nil }
func (s *stubAgentInternalServer) DeregisterJobRequestTopic(_, _ string)                 {}
func (s *stubAgentInternalServer) RegisterJobTerminateTopic(_ string) error              { return nil }
func (s *stubAgentInternalServer) DeregisterJobTerminateTopic(_ string)                  {}
func (s *stubAgentInternalServer) PublishWorkerRegistered(_ context.Context, _ string, _ *emptypb.Empty) error {
	return nil
}
func (s *stubAgentInternalServer) Shutdown() {}
func (s *stubAgentInternalServer) Kill()     {}

// makeIdleWorker creates a Worker with known load and WS_AVAILABLE status.
// conn is nil — safe here because JobRequestAffinity only reads load/status.
func makeIdleWorker(agentName, namespace string, jobType livekit.JobType, load float32) *agent.Worker {
	reg := agent.MakeWorkerRegistration()
	reg.AgentName = agentName
	reg.Namespace = namespace
	reg.JobType = jobType

	w := agent.NewWorker(reg, "key", "secret", nil, logger.GetLogger())

	available := livekit.WorkerStatus_WS_AVAILABLE
	_ = w.HandleUpdateWorker(&livekit.UpdateWorkerStatus{
		Status: &available,
		Load:   load,
	})
	return w
}

// newTestAgentHandler builds a minimal AgentHandler suitable for unit tests
// that only call JobRequestAffinity.
func newTestAgentHandler() *AgentHandler {
	return &AgentHandler{
		agentServer:      &stubAgentInternalServer{},
		logger:           logger.GetLogger(),
		workers:          make(map[string]*agent.Worker),
		namespaceWorkers: make(map[workerKey][]*agent.Worker),
		jobToWorker:      make(map[livekit.JobID]*agent.Worker),
	}
}

func TestJobRequestAffinity(t *testing.T) {
	const agentName = "test-agent"
	const namespace = ""
	jobType := livekit.JobType_JT_ROOM
	key := workerKey{agentName, namespace, jobType}
	job := &livekit.Job{AgentName: agentName, Namespace: namespace, Type: jobType}

	t.Run("single idle worker returns 1.0", func(t *testing.T) {
		h := newTestAgentHandler()
		w := makeIdleWorker(agentName, namespace, jobType, 0.0)
		h.workers[w.ID] = w
		h.namespaceWorkers[key] = []*agent.Worker{w}

		require.InDelta(t, 1.0, h.JobRequestAffinity(context.Background(), job), 0.001)
	})

	t.Run("two idle workers must not exceed 1.0", func(t *testing.T) {
		// With the sum bug both workers contribute 1.0 each → affinity = 2.0,
		// causing this node to always win over any peer with a single worker.
		// The correct value is max(1.0, 1.0) = 1.0.
		h := newTestAgentHandler()
		w1 := makeIdleWorker(agentName, namespace, jobType, 0.0)
		w2 := makeIdleWorker(agentName, namespace, jobType, 0.0)
		h.workers[w1.ID] = w1
		h.workers[w2.ID] = w2
		h.namespaceWorkers[key] = []*agent.Worker{w1, w2}

		affinity := h.JobRequestAffinity(context.Background(), job)
		require.LessOrEqual(t, affinity, float32(1.0),
			"affinity must not exceed 1.0: summing headrooms causes multi-worker nodes to monopolise inter-node selection")
		require.Greater(t, affinity, float32(0.0))
	})

	t.Run("affinity reflects the best worker, not total capacity", func(t *testing.T) {
		// One idle worker (headroom=1.0) and one at 60% load (headroom=0.4).
		// max=1.0, sum=1.4 — the node should report 1.0.
		h := newTestAgentHandler()
		w1 := makeIdleWorker(agentName, namespace, jobType, 0.0)
		w2 := makeIdleWorker(agentName, namespace, jobType, 0.6)
		h.workers[w1.ID] = w1
		h.workers[w2.ID] = w2
		h.namespaceWorkers[key] = []*agent.Worker{w1, w2}

		affinity := h.JobRequestAffinity(context.Background(), job)
		require.InDelta(t, 1.0, affinity, 0.001)
	})

	t.Run("fully loaded workers return 0.0", func(t *testing.T) {
		h := newTestAgentHandler()
		w := makeIdleWorker(agentName, namespace, jobType, 1.0)
		// Mark as WS_FULL so it is not counted
		full := livekit.WorkerStatus_WS_FULL
		_ = w.HandleUpdateWorker(&livekit.UpdateWorkerStatus{Status: &full, Load: 1.0})
		h.workers[w.ID] = w
		h.namespaceWorkers[key] = []*agent.Worker{w}

		require.Equal(t, float32(0.0), h.JobRequestAffinity(context.Background(), job))
	})

	t.Run("workers for different agents are excluded", func(t *testing.T) {
		h := newTestAgentHandler()
		w := makeIdleWorker("other-agent", namespace, jobType, 0.0)
		otherKey := workerKey{"other-agent", namespace, jobType}
		h.workers[w.ID] = w
		h.namespaceWorkers[otherKey] = []*agent.Worker{w}

		require.Equal(t, float32(0.0), h.JobRequestAffinity(context.Background(), job))
	})
}
