package agent_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/atomic"

	"github.com/livekit/livekit-server/pkg/agent"
	"github.com/livekit/livekit-server/pkg/agent/testutils"
	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/protocol/utils/guid"
	"github.com/livekit/protocol/utils/must"
	"github.com/livekit/psrpc"
)

func TestAgent(t *testing.T) {
	testAgentName := "test_agent"
	t.Run("dispatched jobs are assigned to a worker", func(t *testing.T) {
		bus := psrpc.NewLocalMessageBus()

		client := must.Get(rpc.NewAgentInternalClient(bus))
		server := testutils.NewTestServer(bus)
		t.Cleanup(server.Close)

		worker := server.SimulateAgentWorker()
		worker.Register(testAgentName, livekit.JobType_JT_ROOM)
		jobAssignments := worker.JobAssignments.Observe()

		job := &livekit.Job{
			Id:         guid.New(guid.AgentJobPrefix),
			DispatchId: guid.New(guid.AgentDispatchPrefix),
			Type:       livekit.JobType_JT_ROOM,
			Room:       &livekit.Room{},
			AgentName:  testAgentName,
		}
		_, err := client.JobRequest(context.Background(), testAgentName, agent.RoomAgentTopic, job)
		require.NoError(t, err)

		select {
		case a := <-jobAssignments.Events():
			require.EqualValues(t, job.Id, a.Job.Id)
			v, err := auth.ParseAPIToken(a.Token)
			require.NoError(t, err)
			claims, err := v.Verify(server.TestAPISecret)
			require.NoError(t, err)
			require.Equal(t, testAgentName, claims.Attributes[agent.AgentNameAttributeKey])
		case <-time.After(time.Second):
			require.Fail(t, "job assignment timeout")
		}
	})
}

func testBatchJobRequest(t require.TestingT, batchSize int, totalJobs int, client rpc.AgentInternalClient, workers []*testutils.AgentWorker) <-chan struct{} {
	var assigned atomic.Uint32
	done := make(chan struct{})
	for _, w := range workers {
		assignments := w.JobAssignments.Observe()
		go func() {
			defer assignments.Stop()
			for {
				select {
				case <-done:
				case <-assignments.Events():
					if assigned.Inc() == uint32(totalJobs) {
						close(done)
					}
				}
			}
		}()
	}

	// wait for agent registration
	time.Sleep(100 * time.Millisecond)

	var wg sync.WaitGroup
	for i := 0; i < totalJobs; i += batchSize {
		wg.Add(1)
		go func(start int) {
			defer wg.Done()
			for j := start; j < start+batchSize && j < totalJobs; j++ {
				job := &livekit.Job{
					Id:         guid.New(guid.AgentJobPrefix),
					DispatchId: guid.New(guid.AgentDispatchPrefix),
					Type:       livekit.JobType_JT_ROOM,
					Room:       &livekit.Room{},
					AgentName:  "test",
				}
				_, err := client.JobRequest(context.Background(), "test", agent.RoomAgentTopic, job)
				require.NoError(t, err)
			}
		}(i)
	}
	wg.Wait()

	return done
}

func TestAgentLoadBalancing(t *testing.T) {
	t.Run("jobs are distributed normally with baseline worker load", func(t *testing.T) {
		totalWorkers := 5
		totalJobs := 100

		bus := psrpc.NewLocalMessageBus()

		client := must.Get(rpc.NewAgentInternalClient(bus))
		t.Cleanup(client.Close)
		server := testutils.NewTestServer(bus)
		t.Cleanup(server.Close)

		agents := make([]*testutils.AgentWorker, totalWorkers)
		for i := 0; i < totalWorkers; i++ {
			agents[i] = server.SimulateAgentWorker(
				testutils.WithLabel(fmt.Sprintf("agent-%d", i)),
				testutils.WithJobLoad(testutils.NewStableJobLoad(0.01)),
			)
			agents[i].Register("test", livekit.JobType_JT_ROOM)
		}

		select {
		case <-testBatchJobRequest(t, 10, totalJobs, client, agents):
		case <-time.After(time.Second):
			require.Fail(t, "job assignment timeout")
		}

		jobCount := make(map[string]int)
		for _, w := range agents {
			jobCount[w.Label] = len(w.Jobs())
		}

		// check that jobs are distributed normally
		for i := 0; i < totalWorkers; i++ {
			label := fmt.Sprintf("agent-%d", i)
			require.GreaterOrEqual(t, jobCount[label], 0)
			require.Less(t, jobCount[label], 35) // three std deviations from the mean is 32
		}
	})

	t.Run("jobs are distributed with variable and overloaded worker load", func(t *testing.T) {
		totalWorkers := 4
		totalJobs := 15

		bus := psrpc.NewLocalMessageBus()

		client := must.Get(rpc.NewAgentInternalClient(bus))
		t.Cleanup(client.Close)
		server := testutils.NewTestServer(bus)
		t.Cleanup(server.Close)

		agents := make([]*testutils.AgentWorker, totalWorkers)
		for i := 0; i < totalWorkers; i++ {
			label := fmt.Sprintf("agent-%d", i)
			if i%2 == 0 {
				// make sure we have some workers that can accept jobs
				agents[i] = server.SimulateAgentWorker(testutils.WithLabel(label))
			} else {
				agents[i] = server.SimulateAgentWorker(testutils.WithLabel(label), testutils.WithDefaultWorkerLoad(0.9))
			}
			agents[i].Register("test", livekit.JobType_JT_ROOM)
		}

		select {
		case <-testBatchJobRequest(t, 1, totalJobs, client, agents):
		case <-time.After(time.Second):
			require.Fail(t, "job assignment timeout")
		}

		jobCount := make(map[string]int)
		for _, w := range agents {
			jobCount[w.Label] = len(w.Jobs())
		}

		for i := 0; i < totalWorkers; i++ {
			label := fmt.Sprintf("agent-%d", i)

			if i%2 == 0 {
				require.GreaterOrEqual(t, jobCount[label], 2)
			} else {
				require.Equal(t, 0, jobCount[label])
			}
			require.GreaterOrEqual(t, jobCount[label], 0)
		}
	})
}
