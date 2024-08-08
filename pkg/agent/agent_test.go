package agent_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/livekit/livekit-server/pkg/agent"
	"github.com/livekit/livekit-server/pkg/agent/testutil"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/protocol/utils/guid"
	"github.com/livekit/protocol/utils/must"
	"github.com/livekit/psrpc"
)

func TestAgent(t *testing.T) {
	t.Run("dispatched jobs are assigned to a worker", func(t *testing.T) {
		bus := psrpc.NewLocalMessageBus()

		client := must.Get(rpc.NewAgentInternalClient(bus))
		server := testutil.NewTestServer(bus)
		t.Cleanup(server.Close)

		worker := server.SimulateAgentWorker()
		worker.Register("", "test", livekit.JobType_JT_ROOM)
		jobAssignments := worker.JobAssignments.Observe()

		job := &livekit.Job{
			Id:         guid.New(guid.AgentJobPrefix),
			DispatchId: guid.New(guid.AgentDispatchPrefix),
			Type:       livekit.JobType_JT_ROOM,
			Room:       &livekit.Room{},
			Namespace:  "test",
		}
		_, err := client.JobRequest(context.Background(), "test", agent.RoomAgentTopic, job)
		require.NoError(t, err)

		select {
		case a := <-jobAssignments.Events():
			require.EqualValues(t, job.Id, a.Job.Id)
		case <-time.After(time.Second):
			require.Fail(t, "job assignment timeout")
		}
	})
}

func TestAgentLoadBalancing(t *testing.T) {

	batchJobCreate := func(wg *sync.WaitGroup, batchSize int, totalJobs int, client rpc.AgentInternalClient) {
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
						Namespace:  "test",
					}
					_, err := client.JobRequest(context.Background(), "test", agent.RoomAgentTopic, job)
					require.NoError(t, err)
				}
			}(i)
		}
	}

	t.Run("jobs are distributed normally with baseline worker load", func(t *testing.T) {
		totalWorkers := 5
		totalJobs := 100

		bus := psrpc.NewLocalMessageBus()

		client := must.Get(rpc.NewAgentInternalClient(bus))
		server := testutil.NewTestServer(bus)
		t.Cleanup(server.Close)

		agents := make([]*testutil.AgentWorker, totalWorkers)
		for i := 0; i < totalWorkers; i++ {
			agents[i] = server.SimulateAgentWorker()
			agents[i].Register(fmt.Sprintf("agent-%d", i), "test", livekit.JobType_JT_ROOM)
		}

		jobAssignments := make(chan *livekit.Job, totalJobs)
		for i := 0; i < totalWorkers; i++ {
			worker := agents[i]
			go func() {
				for a := range worker.JobAssignments.Observe().Events() {
					jobAssignments <- a.Job
				}
			}()
		}

		var wg sync.WaitGroup
		batchJobCreate(&wg, 10, totalJobs, client)
		wg.Wait()

		jobCount := make(map[string]int)
		for i := 0; i < totalJobs; i++ {
			select {
			case job := <-jobAssignments:
				jobCount[job.AgentName]++
			case <-time.After(time.Second):
				require.Fail(t, "job assignment timeout")
			}
		}

		assignedJobs := 0
		// check that jobs are distributed normally
		for i := 0; i < totalWorkers; i++ {
			agentName := fmt.Sprintf("agent-%d", i)
			assignedJobs += jobCount[agentName]
			require.GreaterOrEqual(t, jobCount[agentName], 0)
			require.Less(t, jobCount[agentName], 35) // three std deviations from the mean is 32
		}

		// ensure all jobs are assigned
		require.Equal(t, 100, assignedJobs)
	})

	t.Run("jobs are distributed with variable and overloaded worker load", func(t *testing.T) {
		totalWorkers := 4
		totalJobs := 15

		bus := psrpc.NewLocalMessageBus()

		client := must.Get(rpc.NewAgentInternalClient(bus))
		server := testutil.NewTestServer(bus)
		t.Cleanup(server.Close)

		agents := make([]*testutil.AgentWorker, totalWorkers)
		for i := 0; i < totalWorkers; i++ {
			if i%2 == 0 {
				// make sure we have some workers that can accept jobs
				agents[i] = server.SimulateAgentWorker()
			} else {
				agents[i] = server.SimulateAgentWorker(testutil.WithDefaultWorkerLoad(0.9))
			}
			agents[i].Register(fmt.Sprintf("agent-%d", i), "test", livekit.JobType_JT_ROOM)
		}

		jobAssignments := make(chan *livekit.Job, totalJobs)
		for i := 0; i < totalWorkers; i++ {
			worker := agents[i]
			go func() {
				for a := range worker.JobAssignments.Observe().Events() {
					jobAssignments <- a.Job
				}
			}()
		}

		var wg sync.WaitGroup
		batchJobCreate(&wg, 1, totalJobs, client)
		wg.Wait()

		jobCount := make(map[string]int)
		for i := 0; i < totalJobs; i++ {
			select {
			case job := <-jobAssignments:
				jobCount[job.AgentName]++
			case <-time.After(time.Second):
				require.Fail(t, "job assignment timeout")
			}
		}

		assignedJobs := 0
		for i := 0; i < totalWorkers; i++ {
			agentName := fmt.Sprintf("agent-%d", i)
			assignedJobs += jobCount[agentName]

			if i%2 == 0 {
				require.GreaterOrEqual(t, jobCount[agentName], 2)
			} else {
				require.Equal(t, 0, jobCount[agentName])
			}
			require.GreaterOrEqual(t, jobCount[agentName], 0)
		}

		// ensure all jobs are assigned
		require.Equal(t, 15, assignedJobs)
	})
}
