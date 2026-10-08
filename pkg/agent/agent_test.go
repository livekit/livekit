package agent_test

import (
	"context"
	"errors"
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
			_, claims, err := v.Verify(server.TestAPISecret)
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
		for i := range totalWorkers {
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
		for i := range totalWorkers {
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
		for i := range totalWorkers {
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

		for i := range totalWorkers {
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

func TestJobTerminateReleasesJob(t *testing.T) {
	const agentName = "test_agent"

	// starts a server with a worker whose job request topic is registered
	newWorker := func(t *testing.T) (rpc.AgentInternalClient, *testutils.AgentWorker) {
		bus := psrpc.NewLocalMessageBus()
		client := must.Get(rpc.NewAgentInternalClient(bus))
		server := testutils.NewTestServer(bus)
		t.Cleanup(server.Close)

		registered := must.Get(client.SubscribeWorkerRegistered(context.Background(), agent.DefaultHandlerNamespace))
		defer registered.Close()

		worker := server.SimulateAgentWorker()
		worker.Register(agentName, livekit.JobType_JT_ROOM)
		select {
		case <-registered.Channel():
		case <-time.After(time.Second):
			require.Fail(t, "registration timeout")
		}
		return client, worker
	}

	// assigns a job, then terminates it as Room.RemoveParticipant does when
	// the agent participant leaves
	runJob := func(t *testing.T, client rpc.AgentInternalClient) *livekit.Job {
		job := &livekit.Job{
			Id:         guid.New(guid.AgentJobPrefix),
			DispatchId: guid.New(guid.AgentDispatchPrefix),
			Type:       livekit.JobType_JT_ROOM,
			Room:       &livekit.Room{},
			AgentName:  agentName,
		}
		_, err := client.JobRequest(context.Background(), agentName, agent.RoomAgentTopic, job)
		require.NoError(t, err)

		res, err := client.JobTerminate(context.Background(), job.Id, &rpc.JobTerminateRequest{
			JobId:  job.Id,
			Reason: rpc.JobTerminateReason_AGENT_LEFT_ROOM,
		})
		require.NoError(t, err)
		require.Equal(t, livekit.JobStatus_JS_FAILED, res.State.Status)
		return job
	}

	// no server should still be handling JobTerminate for the job
	requireReleased := func(t *testing.T, client rpc.AgentInternalClient, job *livekit.Job) {
		_, err := client.JobTerminate(context.Background(), job.Id, &rpc.JobTerminateRequest{JobId: job.Id}, psrpc.WithRequestTimeout(500*time.Millisecond))
		require.ErrorIs(t, err, psrpc.ErrNoResponse)
	}

	t.Run("worker does not report job status", func(t *testing.T) {
		// workers are not required to report job status after a termination
		// (e.g. agents-js does not currently send UpdateJobStatus), and a
		// worker disconnect does not release jobs that are no longer running
		client, worker := newWorker(t)
		job := runJob(t, client)
		require.NoError(t, worker.Close())
		requireReleased(t, client, job)
	})

	t.Run("worker reports ended status after termination", func(t *testing.T) {
		client, worker := newWorker(t)
		job := runJob(t, client)
		worker.SendUpdateJob(&livekit.UpdateJobStatus{
			JobId:  job.Id,
			Status: livekit.JobStatus_JS_SUCCESS,
		})

		// worker messages are handled in order, so the pong confirms the
		// update was handled
		pongs := worker.WorkerPongs.Observe()
		defer pongs.Stop()
		worker.SendPing(&livekit.WorkerPing{})
		select {
		case <-pongs.Events():
		case <-time.After(time.Second):
			require.Fail(t, "pong timeout")
		}

		require.NoError(t, worker.Close())
		requireReleased(t, client, job)
	})
}

func TestConnectionClosedOnDispatchError(t *testing.T) {
	t.Run("connection closed when unknown message type received", func(t *testing.T) {
		bus := psrpc.NewLocalMessageBus()
		server := testutils.NewTestServer(bus)
		t.Cleanup(server.Close)

		// register agent
		worker := server.SimulateAgentWorker()
		worker.Register("test_agent", livekit.JobType_JT_ROOM)
		responses := worker.RegisterWorkerResponses.Observe()
		select {
		case <-responses.Events():
			// registered
		case <-time.After(time.Second):
			require.Fail(t, "registration timeout")
		}
		responses.Stop()

		// send invalid message (nil Message field triggers ErrUnknownWorkerSignal)
		worker.SendMessage(&livekit.WorkerMessage{Message: nil})

		select {
		case <-worker.Closed():
			// connection closed
		case <-time.After(time.Second):
			require.Fail(t, "connection should have been closed after dispatch error")
		}
	})
}

func TestDrainConnectionsDoesNotDeadlock(t *testing.T) {
	for _, force := range []bool{false, true} {
		t.Run(fmt.Sprintf("force=%v", force), func(t *testing.T) {
			bus := psrpc.NewLocalMessageBus()
			server := testutils.NewTestServer(bus)
			t.Cleanup(server.Close)

			worker := server.SimulateAgentWorker()
			worker.Register("drain_agent", livekit.JobType_JT_ROOM)
			responses := worker.RegisterWorkerResponses.Observe()
			select {
			case <-responses.Events():
			case <-time.After(time.Second):
				require.Fail(t, "registration timeout")
			}
			responses.Stop()

			done := make(chan struct{})
			go func() {
				server.DrainConnections(time.Millisecond, force)
				close(done)
			}()

			select {
			case <-done:
			case <-time.After(5 * time.Second):
				require.Fail(t, "DrainConnections deadlocked while closing workers")
			}

			select {
			case <-worker.Closed():
			case <-time.After(time.Second):
				require.Fail(t, "worker should be closed after drain")
			}
		})
	}
}

func TestJobTerminateHandlerReleased(t *testing.T) {
	const jobCount = 50

	requestJobs := func(t *testing.T, client rpc.AgentInternalClient, agentName func(i int) string) []string {
		jobIDs := make([]string, jobCount)
		errs := make([]error, jobCount)
		var wg sync.WaitGroup
		for i := range jobCount {
			job := &livekit.Job{
				Id:         guid.New(guid.AgentJobPrefix),
				DispatchId: guid.New(guid.AgentDispatchPrefix),
				Type:       livekit.JobType_JT_ROOM,
				Room:       &livekit.Room{},
				AgentName:  agentName(i),
			}
			jobIDs[i] = job.Id
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, errs[i] = client.JobRequest(context.Background(), job.AgentName, agent.RoomAgentTopic, job)
			}()
		}
		wg.Wait()
		for _, err := range errs {
			require.NoError(t, err)
		}
		return jobIDs
	}

	// the server reads the ping only after registering the worker's job request topic.
	waitRegistered := func(t *testing.T, w *testutils.AgentWorker, agentName string) {
		pongs := w.WorkerPongs.Observe()
		defer pongs.Stop()
		w.Register(agentName, livekit.JobType_JT_ROOM)
		w.SendPing(&livekit.WorkerPing{Timestamp: time.Now().UnixMilli()})
		select {
		case <-pongs.Events():
		case <-time.After(5 * time.Second):
			require.Fail(t, "registration timeout")
		}
	}

	// any response means a JobTerminate handler is still registered.
	countAnswered := func(client rpc.AgentInternalClient, jobIDs []string) int32 {
		var answered atomic.Int32
		var wg sync.WaitGroup
		for _, id := range jobIDs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := client.JobTerminate(context.Background(), id, &rpc.JobTerminateRequest{JobId: id}, psrpc.WithRequestTimeout(200*time.Millisecond))
				if !errors.Is(err, psrpc.ErrRequestTimedOut) && !errors.Is(err, psrpc.ErrNoResponse) {
					answered.Inc()
				}
			}()
		}
		wg.Wait()
		return answered.Load()
	}

	// handlers are released as the server processes the worker's messages; a leaked one never is.
	requireHandlersReleased := func(t *testing.T, client rpc.AgentInternalClient, jobIDs []string) {
		var answered int32
		require.Eventually(t, func() bool {
			answered = countAnswered(client, jobIDs)
			return answered == 0
		}, 10*time.Second, 100*time.Millisecond, "jobs with leaked JobTerminate handlers: %d", answered)
	}

	t.Run("job fails on assignment", func(t *testing.T) {
		bus := psrpc.NewLocalMessageBus()
		client := must.Get(rpc.NewAgentInternalClient(bus))
		server := testutils.NewTestServer(bus)
		t.Cleanup(server.Close)

		var worker *testutils.AgentWorker
		worker = server.SimulateAgentWorker(testutils.WithJobAssignmentHandler(func(j *livekit.Job) testutils.JobLoad {
			worker.SendUpdateJob(&livekit.UpdateJobStatus{JobId: j.Id, Status: livekit.JobStatus_JS_FAILED})
			return testutils.NewStableJobLoad(0)
		}))
		waitRegistered(t, worker, "fail_agent")

		jobIDs := requestJobs(t, client, func(int) string { return "fail_agent" })

		requireHandlersReleased(t, client, jobIDs)
	})

	t.Run("worker disconnects on assignment", func(t *testing.T) {
		bus := psrpc.NewLocalMessageBus()
		client := must.Get(rpc.NewAgentInternalClient(bus))
		server := testutils.NewTestServer(bus)
		t.Cleanup(server.Close)

		agentName := func(i int) string { return fmt.Sprintf("disconnect_agent_%d", i) }
		for i := range jobCount {
			var worker *testutils.AgentWorker
			worker = server.SimulateAgentWorker(testutils.WithJobAssignmentHandler(func(j *livekit.Job) testutils.JobLoad {
				worker.Close()
				return nil
			}))
			waitRegistered(t, worker, agentName(i))
		}

		jobIDs := requestJobs(t, client, agentName)

		requireHandlersReleased(t, client, jobIDs)
	})
}
