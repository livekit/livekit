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
	"google.golang.org/protobuf/proto"

	"github.com/livekit/livekit-server/pkg/agent"
	"github.com/livekit/livekit-server/pkg/agent/testutils"
	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
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

func TestJobReleasedWhenItEndsDuringAssignment(t *testing.T) {
	const agentName = "test_agent"

	// there is a single server, so don't wait for other servers' affinity
	firstAvailable := psrpc.WithSelectionOpts(psrpc.SelectionOpts{
		AcceptFirstAvailable: true,
		AffinityTimeout:      5 * time.Second,
	})

	newServer := func(t *testing.T) (rpc.AgentInternalClient, *testutils.TestServer) {
		bus := psrpc.NewLocalMessageBus()
		client := must.Get(rpc.NewAgentInternalClient(bus))
		server := testutils.NewTestServer(bus)
		t.Cleanup(server.Close)
		return client, server
	}

	// registers a worker whose job request topic is registered
	registerWorker := func(t *testing.T, client rpc.AgentInternalClient, server *testutils.TestServer, opts ...testutils.SimulatedWorkerOption) *testutils.AgentWorker {
		registered := must.Get(client.SubscribeWorkerRegistered(context.Background(), agent.DefaultHandlerNamespace))
		defer registered.Close()

		worker := server.SimulateAgentWorker(opts...)
		worker.Register(agentName, livekit.JobType_JT_ROOM)
		select {
		case <-registered.Channel():
		case <-time.After(time.Second):
			require.Fail(t, "registration timeout")
		}
		return worker
	}

	requestJob := func(client rpc.AgentInternalClient) (*livekit.Job, error) {
		job := &livekit.Job{
			Id:         guid.New(guid.AgentJobPrefix),
			DispatchId: guid.New(guid.AgentDispatchPrefix),
			Type:       livekit.JobType_JT_ROOM,
			Room:       &livekit.Room{},
			AgentName:  agentName,
		}
		_, err := client.JobRequest(context.Background(), agentName, agent.RoomAgentTopic, job, firstAvailable)
		return job, err
	}

	// a released job has no JobTerminate handler, so nothing responds to it
	requireReleased := func(t *testing.T, client rpc.AgentInternalClient, jobs []*livekit.Job) {
		errs := make([]error, len(jobs))
		var wg sync.WaitGroup
		for i, job := range jobs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := client.JobTerminate(context.Background(), job.Id, &rpc.JobTerminateRequest{JobId: job.Id}, psrpc.WithRequestTimeout(500*time.Millisecond))
				if !errors.Is(err, psrpc.ErrNoResponse) {
					errs[i] = fmt.Errorf("job %s: JobTerminate handler still registered: %v", job.Id, err)
				}
			}()
		}
		wg.Wait()
		require.NoError(t, errors.Join(errs...))
	}

	t.Run("worker ends job on assignment", func(t *testing.T) {
		client, server := newServer(t)

		const jobCount = 200

		var worker *testutils.AgentWorker
		updated := make(chan struct{}, jobCount)
		worker = registerWorker(t, client, server, testutils.WithJobAssignmentHandler(func(job *livekit.Job) testutils.JobLoad {
			worker.SendUpdateJob(&livekit.UpdateJobStatus{
				JobId:  job.Id,
				Status: livekit.JobStatus_JS_FAILED,
			})
			updated <- struct{}{}
			return testutils.NewStableJobLoad(0)
		}))

		// only a share of the jobs hit the window (see #4901), so run a batch
		jobs := make([]*livekit.Job, jobCount)
		for i := range jobs {
			job, err := requestJob(client)
			require.NoError(t, err)
			jobs[i] = job
		}

		for range jobs {
			select {
			case <-updated:
			case <-time.After(5 * time.Second):
				require.Fail(t, "timed out waiting for job status updates")
			}
		}

		// worker messages are handled in order, so the pong confirms every
		// status update was handled
		pongs := worker.WorkerPongs.Observe()
		defer pongs.Stop()
		worker.SendPing(&livekit.WorkerPing{})
		select {
		case <-pongs.Events():
		case <-time.After(5 * time.Second):
			require.Fail(t, "pong timeout")
		}

		requireReleased(t, client, jobs)
	})

	t.Run("worker disconnects on assignment", func(t *testing.T) {
		client, server := newServer(t)

		// only the first worker of an agent announces its registration, later
		// ones simply are not selected until they are registered. only a share
		// of the jobs hit the window (see #4901), so run a batch.
		workers := make([]*testutils.AgentWorker, 20)
		for i := range workers {
			var worker *testutils.AgentWorker
			opt := testutils.WithJobAssignmentHandler(func(*livekit.Job) testutils.JobLoad {
				_ = worker.Close()
				return nil
			})
			if i == 0 {
				worker = registerWorker(t, client, server, opt)
			} else {
				worker = server.SimulateAgentWorker(opt)
				worker.Register(agentName, livekit.JobType_JT_ROOM)
			}
			workers[i] = worker
		}

		// a failed check is inconclusive, so it is not reported as disabled
		roomEnabled := func() (bool, error) {
			res, err := client.CheckEnabled(context.Background(), &rpc.CheckEnabledRequest{}, psrpc.WithRequestTimeout(time.Second))
			if err != nil {
				return false, err
			}
			enabled := false
			for r := range res {
				if r.Err != nil {
					err = r.Err
				} else if r.Result.GetRoomEnabled() {
					enabled = true
				}
			}
			return enabled, err
		}

		enabled, err := roomEnabled()
		require.NoError(t, err)
		require.True(t, enabled, "no worker was registered")

		// requests can fail once the workers are gone
		var jobs []*livekit.Job
		var mu sync.Mutex
		var wg sync.WaitGroup
		for range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if job, err := requestJob(client); err == nil {
					mu.Lock()
					jobs = append(jobs, job)
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		require.GreaterOrEqual(t, len(jobs), len(workers)/2, "too few jobs were assigned")

		for _, worker := range workers {
			_ = worker.Close()
		}

		// every worker has been deregistered once no job type is enabled
		require.Eventually(t, func() bool {
			enabled, err := roomEnabled()
			return err == nil && !enabled
		}, 10*time.Second, 20*time.Millisecond, "workers were not deregistered")

		requireReleased(t, client, jobs)
	})
}

type assignmentConn struct {
	availability chan *livekit.AvailabilityRequest
}

func (c *assignmentConn) WriteServerMessage(msg *livekit.ServerMessage) (int, error) {
	if a := msg.GetAvailability(); a != nil {
		c.availability <- a
		return 0, nil
	}

	// WSSignalConnection marshals the message outside the worker's lock
	b, err := proto.Marshal(msg)
	return len(b), err
}

func (c *assignmentConn) ReadWorkerMessage() (*livekit.WorkerMessage, int, error) {
	return nil, 0, errors.New("not implemented")
}

func (c *assignmentConn) SetReadDeadline(time.Time) error { return nil }
func (c *assignmentConn) Close() error                    { return nil }
func (c *assignmentConn) CloseWithReason(string) error    { return nil }

func TestAssignJobTracksJobBeforeSendingIt(t *testing.T) {
	// the worker knows the job ID from the availability request, so it can
	// report the job ended before the assignment write returns
	assignJob := func(t *testing.T, assertEnded bool, endJob func(worker *agent.Worker, jobID string)) {
		conn := &assignmentConn{availability: make(chan *livekit.AvailabilityRequest, 1)}
		registration := agent.MakeWorkerRegistration()
		registration.Permissions = &livekit.ParticipantPermission{}
		worker := agent.NewWorker(registration, "test", "verysecretsecret", conn, logger.GetLogger())
		t.Cleanup(worker.Close)

		job := &livekit.Job{
			Id:        guid.New(guid.AgentJobPrefix),
			Type:      livekit.JobType_JT_ROOM,
			Room:      &livekit.Room{Name: "test_room"},
			AgentName: "test_agent",
		}

		go func() {
			<-conn.availability
			_ = worker.HandleAvailability(&livekit.AvailabilityResponse{
				JobId:               job.Id,
				Available:           true,
				ParticipantIdentity: "test_agent",
			})
		}()

		hook := func(next func(*livekit.JobAssignment) error) func(*livekit.JobAssignment) error {
			return func(a *livekit.JobAssignment) error {
				endJob(worker, job.Id)
				return next(a)
			}
		}

		_, err := worker.AssignJob(context.Background(), job, hook)
		require.NoError(t, err)

		if assertEnded {
			_, err = worker.GetJobState(livekit.JobID(job.Id))
			require.ErrorIs(t, err, agent.ErrJobNotFound, "ended job is still running")
		}
	}

	t.Run("status update handled while the assignment is sent", func(t *testing.T) {
		assignJob(t, true, func(worker *agent.Worker, jobID string) {
			require.NoError(t, worker.HandleUpdateJob(&livekit.UpdateJobStatus{
				JobId:  jobID,
				Status: livekit.JobStatus_JS_FAILED,
			}))
		})
	})

	t.Run("job is running after a plain assignment", func(t *testing.T) {
		assignJob(t, false, func(worker *agent.Worker, jobID string) {
			// no status update, so the job stays running
			t.Cleanup(func() {
				_, err := worker.GetJobState(livekit.JobID(jobID))
				require.NoError(t, err, "assigned job is not running")
			})
		})
	})

	t.Run("status update handled while the assignment is marshalled", func(t *testing.T) {
		// the assignment must not be marshalled while the update writes to it
		var wg sync.WaitGroup
		t.Cleanup(wg.Wait)
		assignJob(t, false, func(worker *agent.Worker, jobID string) {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_ = worker.HandleUpdateJob(&livekit.UpdateJobStatus{
					JobId:  jobID,
					Status: livekit.JobStatus_JS_FAILED,
				})
			}()
		})
	})
}
