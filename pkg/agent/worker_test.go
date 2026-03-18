package agent

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/protocol/utils/guid"
)

// --- Mock SignalConn ---

type mockSignalConn struct {
	mu       sync.Mutex
	messages []*livekit.ServerMessage
	closed   bool
	writeErr error
	onWrite  func(*livekit.ServerMessage)
}

func newMockSignalConn() *mockSignalConn {
	return &mockSignalConn{}
}

func (m *mockSignalConn) WriteServerMessage(msg *livekit.ServerMessage) (int, error) {
	m.mu.Lock()
	if m.writeErr != nil {
		err := m.writeErr
		m.mu.Unlock()
		return 0, err
	}
	m.messages = append(m.messages, msg)
	cb := m.onWrite
	m.mu.Unlock()

	if cb != nil {
		cb(msg)
	}
	return 0, nil
}

func (m *mockSignalConn) ReadWorkerMessage() (*livekit.WorkerMessage, int, error) {
	return nil, 0, nil
}

func (m *mockSignalConn) SetReadDeadline(_ time.Time) error {
	return nil
}

func (m *mockSignalConn) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	return nil
}

func (m *mockSignalConn) getMessages() []*livekit.ServerMessage {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*livekit.ServerMessage, len(m.messages))
	copy(out, m.messages)
	return out
}

func (m *mockSignalConn) isClosed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closed
}

// --- Dispatch recorder for DispatchWorkerSignal tests ---

type dispatchRecorder struct {
	UnimplementedWorkerSignalHandler
	lastCall string
}

func (h *dispatchRecorder) HandleRegister(*livekit.RegisterWorkerRequest) error {
	h.lastCall = "register"
	return nil
}

func (h *dispatchRecorder) HandleAvailability(*livekit.AvailabilityResponse) error {
	h.lastCall = "availability"
	return nil
}

func (h *dispatchRecorder) HandleUpdateJob(*livekit.UpdateJobStatus) error {
	h.lastCall = "updateJob"
	return nil
}

func (h *dispatchRecorder) HandleSimulateJob(*livekit.SimulateJobRequest) error {
	h.lastCall = "simulateJob"
	return nil
}

func (h *dispatchRecorder) HandlePing(*livekit.WorkerPing) error {
	h.lastCall = "ping"
	return nil
}

func (h *dispatchRecorder) HandleUpdateWorker(*livekit.UpdateWorkerStatus) error {
	h.lastCall = "updateWorker"
	return nil
}

func (h *dispatchRecorder) HandleMigrateJob(*livekit.MigrateJobRequest) error {
	h.lastCall = "migrateJob"
	return nil
}

// --- Test helpers ---

func newTestWorker(conn SignalConn) *Worker {
	reg := MakeWorkerRegistration()
	reg.AgentName = "test-agent"
	reg.JobType = livekit.JobType_JT_ROOM
	reg.Namespace = "test-ns"
	reg.Permissions = &livekit.ParticipantPermission{
		CanSubscribe:      true,
		CanPublish:        true,
		CanPublishData:    true,
		CanUpdateMetadata: true,
	}
	return NewWorker(reg, "test", "verysecretsecret", conn, logger.GetLogger())
}

func makeTestJob() *livekit.Job {
	return &livekit.Job{
		Id:        guid.New(guid.AgentJobPrefix),
		Type:      livekit.JobType_JT_ROOM,
		Room:      &livekit.Room{Name: "test-room"},
		AgentName: "test-agent",
	}
}

func makeTestJobWithState() *livekit.Job {
	return &livekit.Job{
		Id:        guid.New(guid.AgentJobPrefix),
		Type:      livekit.JobType_JT_ROOM,
		Room:      &livekit.Room{Name: "test-room"},
		AgentName: "test-agent",
		State: &livekit.JobState{
			Status: livekit.JobStatus_JS_RUNNING,
		},
	}
}

func addRunningJob(w *Worker, job *livekit.Job) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.runningJobs[livekit.JobID(job.Id)] = job
}

func respondToAvailability(conn *mockSignalConn, w *Worker, available bool, terminate bool) {
	conn.mu.Lock()
	conn.onWrite = func(msg *livekit.ServerMessage) {
		if avail, ok := msg.Message.(*livekit.ServerMessage_Availability); ok {
			go func() {
				_ = w.HandleAvailability(&livekit.AvailabilityResponse{
					JobId:               avail.Availability.Job.Id,
					Available:           available,
					Terminate:           terminate,
					ParticipantIdentity: "agent-identity",
					ParticipantName:     "agent-name",
				})
			}()
		}
	}
	conn.mu.Unlock()
}

// ========== Tests ==========

func TestJobStatusIsEnded(t *testing.T) {
	require.True(t, JobStatusIsEnded(livekit.JobStatus_JS_SUCCESS))
	require.True(t, JobStatusIsEnded(livekit.JobStatus_JS_FAILED))
	require.False(t, JobStatusIsEnded(livekit.JobStatus_JS_RUNNING))
	require.False(t, JobStatusIsEnded(livekit.JobStatus_JS_PENDING))
}

func TestDispatchWorkerSignal(t *testing.T) {
	h := &dispatchRecorder{}

	tests := []struct {
		name     string
		msg      *livekit.WorkerMessage
		expected string
	}{
		{
			"register",
			&livekit.WorkerMessage{Message: &livekit.WorkerMessage_Register{Register: &livekit.RegisterWorkerRequest{}}},
			"register",
		},
		{
			"availability",
			&livekit.WorkerMessage{Message: &livekit.WorkerMessage_Availability{Availability: &livekit.AvailabilityResponse{}}},
			"availability",
		},
		{
			"update job",
			&livekit.WorkerMessage{Message: &livekit.WorkerMessage_UpdateJob{UpdateJob: &livekit.UpdateJobStatus{}}},
			"updateJob",
		},
		{
			"simulate job",
			&livekit.WorkerMessage{Message: &livekit.WorkerMessage_SimulateJob{SimulateJob: &livekit.SimulateJobRequest{}}},
			"simulateJob",
		},
		{
			"ping",
			&livekit.WorkerMessage{Message: &livekit.WorkerMessage_Ping{Ping: &livekit.WorkerPing{}}},
			"ping",
		},
		{
			"update worker",
			&livekit.WorkerMessage{Message: &livekit.WorkerMessage_UpdateWorker{UpdateWorker: &livekit.UpdateWorkerStatus{}}},
			"updateWorker",
		},
		{
			"migrate job",
			&livekit.WorkerMessage{Message: &livekit.WorkerMessage_MigrateJob{MigrateJob: &livekit.MigrateJobRequest{}}},
			"migrateJob",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h.lastCall = ""
			err := DispatchWorkerSignal(tt.msg, h)
			require.NoError(t, err)
			require.Equal(t, tt.expected, h.lastCall)
		})
	}

	t.Run("unknown message type", func(t *testing.T) {
		err := DispatchWorkerSignal(&livekit.WorkerMessage{}, h)
		require.ErrorIs(t, err, ErrUnknownWorkerSignal)
	})
}

func TestUnimplementedWorkerSignalHandler(t *testing.T) {
	h := UnimplementedWorkerSignalHandler{}
	require.ErrorIs(t, h.HandleRegister(nil), ErrUnimplementedWrorkerSignal)
	require.ErrorIs(t, h.HandleAvailability(nil), ErrUnimplementedWrorkerSignal)
	require.ErrorIs(t, h.HandleUpdateJob(nil), ErrUnimplementedWrorkerSignal)
	require.ErrorIs(t, h.HandleSimulateJob(nil), ErrUnimplementedWrorkerSignal)
	require.ErrorIs(t, h.HandlePing(nil), ErrUnimplementedWrorkerSignal)
	require.ErrorIs(t, h.HandleUpdateWorker(nil), ErrUnimplementedWrorkerSignal)
	require.ErrorIs(t, h.HandleMigrateJob(nil), ErrUnimplementedWrorkerSignal)
}

func TestWorkerPingHandler(t *testing.T) {
	conn := newMockSignalConn()
	h := WorkerPingHandler{conn: conn}

	now := time.Now().UnixMilli()
	err := h.HandlePing(&livekit.WorkerPing{Timestamp: now})
	require.NoError(t, err)

	msgs := conn.getMessages()
	require.Len(t, msgs, 1)
	pong, ok := msgs[0].Message.(*livekit.ServerMessage_Pong)
	require.True(t, ok)
	require.Equal(t, now, pong.Pong.LastTimestamp)
	require.GreaterOrEqual(t, pong.Pong.Timestamp, now)
}

func TestMakeWorkerRegistration(t *testing.T) {
	reg := MakeWorkerRegistration()
	require.NotEmpty(t, reg.ID)
	require.Equal(t, WorkerProtocolVersion(CurrentProtocol), reg.Protocol)

	reg2 := MakeWorkerRegistration()
	require.NotEqual(t, reg.ID, reg2.ID)
}

func TestWorkerRegisterer(t *testing.T) {
	t.Run("successful registration", func(t *testing.T) {
		conn := newMockSignalConn()
		base := MakeWorkerRegistration()
		serverInfo := &livekit.ServerInfo{}
		reg := NewWorkerRegisterer(conn, serverInfo, base)

		require.False(t, reg.Registered())

		err := reg.HandleRegister(&livekit.RegisterWorkerRequest{
			Type:      livekit.JobType_JT_ROOM,
			AgentName: "test-agent",
			Version:   "1.0",
		})
		require.NoError(t, err)
		require.True(t, reg.Registered())

		r := reg.Registration()
		require.Equal(t, "test-agent", r.AgentName)
		require.Equal(t, "1.0", r.Version)
		require.Equal(t, livekit.JobType_JT_ROOM, r.JobType)

		msgs := conn.getMessages()
		require.Len(t, msgs, 1)
		regResp, ok := msgs[0].Message.(*livekit.ServerMessage_Register)
		require.True(t, ok)
		require.Equal(t, base.ID, regResp.Register.WorkerId)
		require.Equal(t, serverInfo, regResp.Register.ServerInfo)
	})

	t.Run("unknown job type", func(t *testing.T) {
		conn := newMockSignalConn()
		reg := NewWorkerRegisterer(conn, nil, MakeWorkerRegistration())

		err := reg.HandleRegister(&livekit.RegisterWorkerRequest{
			Type: livekit.JobType(999),
		})
		require.ErrorIs(t, err, ErrUnknownJobType)
		require.False(t, reg.Registered())
	})

	t.Run("default permissions when nil", func(t *testing.T) {
		conn := newMockSignalConn()
		reg := NewWorkerRegisterer(conn, nil, MakeWorkerRegistration())

		err := reg.HandleRegister(&livekit.RegisterWorkerRequest{
			Type:               livekit.JobType_JT_ROOM,
			AllowedPermissions: nil,
		})
		require.NoError(t, err)

		perms := reg.Registration().Permissions
		require.NotNil(t, perms)
		require.True(t, perms.CanSubscribe)
		require.True(t, perms.CanPublish)
		require.True(t, perms.CanPublishData)
		require.True(t, perms.CanUpdateMetadata)
	})

	t.Run("custom permissions", func(t *testing.T) {
		conn := newMockSignalConn()
		reg := NewWorkerRegisterer(conn, nil, MakeWorkerRegistration())

		customPerms := &livekit.ParticipantPermission{
			CanSubscribe: true,
			CanPublish:   false,
		}
		err := reg.HandleRegister(&livekit.RegisterWorkerRequest{
			Type:               livekit.JobType_JT_ROOM,
			AllowedPermissions: customPerms,
		})
		require.NoError(t, err)
		require.Equal(t, customPerms, reg.Registration().Permissions)
	})

	t.Run("deadline is set", func(t *testing.T) {
		conn := newMockSignalConn()
		before := time.Now()
		reg := NewWorkerRegisterer(conn, nil, MakeWorkerRegistration())

		d := reg.Deadline()
		require.True(t, d.After(before))
		require.True(t, d.Before(before.Add(RegisterTimeout+time.Second)))
	})
}

func TestNewWorker(t *testing.T) {
	conn := newMockSignalConn()
	w := newTestWorker(conn)
	t.Cleanup(func() { w.Close() })

	require.Equal(t, "test-agent", w.AgentName)
	require.Equal(t, livekit.JobType_JT_ROOM, w.JobType)
	require.Equal(t, "test-ns", w.Namespace)
	require.NotEmpty(t, w.ID)
	require.NotNil(t, w.Logger())
	require.False(t, w.IsClosed())
	require.Equal(t, livekit.WorkerStatus(0), w.Status())
	require.Equal(t, float32(0), w.Load())
	require.Empty(t, w.RunningJobs())
	require.Equal(t, 0, w.RunningJobCount())
}

func TestWorkerStatus(t *testing.T) {
	conn := newMockSignalConn()
	w := newTestWorker(conn)
	t.Cleanup(func() { w.Close() })

	require.Equal(t, livekit.WorkerStatus(0), w.Status())

	status := livekit.WorkerStatus_WS_AVAILABLE
	err := w.HandleUpdateWorker(&livekit.UpdateWorkerStatus{Status: &status})
	require.NoError(t, err)
	require.Equal(t, livekit.WorkerStatus_WS_AVAILABLE, w.Status())

	status2 := livekit.WorkerStatus_WS_FULL
	err = w.HandleUpdateWorker(&livekit.UpdateWorkerStatus{Status: &status2})
	require.NoError(t, err)
	require.Equal(t, livekit.WorkerStatus_WS_FULL, w.Status())
}

func TestWorkerLoad(t *testing.T) {
	conn := newMockSignalConn()
	w := newTestWorker(conn)
	t.Cleanup(func() { w.Close() })

	require.Equal(t, float32(0), w.Load())

	err := w.HandleUpdateWorker(&livekit.UpdateWorkerStatus{Load: 0.75})
	require.NoError(t, err)
	require.Equal(t, float32(0.75), w.Load())
}

func TestWorkerRunningJobs(t *testing.T) {
	t.Run("empty initially", func(t *testing.T) {
		conn := newMockSignalConn()
		w := newTestWorker(conn)
		t.Cleanup(func() { w.Close() })

		jobs := w.RunningJobs()
		require.Empty(t, jobs)
	})

	t.Run("returns jobs", func(t *testing.T) {
		conn := newMockSignalConn()
		w := newTestWorker(conn)
		t.Cleanup(func() { w.Close() })

		job := makeTestJobWithState()
		addRunningJob(w, job)

		jobs := w.RunningJobs()
		require.Len(t, jobs, 1)
		require.Equal(t, job, jobs[livekit.JobID(job.Id)])
	})

	t.Run("returns a copy of the map", func(t *testing.T) {
		conn := newMockSignalConn()
		w := newTestWorker(conn)
		t.Cleanup(func() { w.Close() })

		job := makeTestJobWithState()
		addRunningJob(w, job)

		jobs := w.RunningJobs()
		jobs[livekit.JobID("fake-id")] = &livekit.Job{}

		require.Equal(t, 1, w.RunningJobCount())
	})
}

func TestWorkerRunningJobCount(t *testing.T) {
	conn := newMockSignalConn()
	w := newTestWorker(conn)
	t.Cleanup(func() { w.Close() })

	require.Equal(t, 0, w.RunningJobCount())

	addRunningJob(w, makeTestJobWithState())
	require.Equal(t, 1, w.RunningJobCount())

	addRunningJob(w, makeTestJobWithState())
	require.Equal(t, 2, w.RunningJobCount())
}

func TestWorkerGetJobState(t *testing.T) {
	t.Run("existing job", func(t *testing.T) {
		conn := newMockSignalConn()
		w := newTestWorker(conn)
		t.Cleanup(func() { w.Close() })

		job := makeTestJobWithState()
		addRunningJob(w, job)

		state, err := w.GetJobState(livekit.JobID(job.Id))
		require.NoError(t, err)
		require.NotNil(t, state)
		require.Equal(t, livekit.JobStatus_JS_RUNNING, state.Status)
	})

	t.Run("non-existing job", func(t *testing.T) {
		conn := newMockSignalConn()
		w := newTestWorker(conn)
		t.Cleanup(func() { w.Close() })

		_, err := w.GetJobState("nonexistent")
		require.ErrorIs(t, err, ErrJobNotFound)
	})

	t.Run("returns a deep clone", func(t *testing.T) {
		conn := newMockSignalConn()
		w := newTestWorker(conn)
		t.Cleanup(func() { w.Close() })

		job := makeTestJobWithState()
		addRunningJob(w, job)

		state, err := w.GetJobState(livekit.JobID(job.Id))
		require.NoError(t, err)

		state.Status = livekit.JobStatus_JS_FAILED

		state2, err := w.GetJobState(livekit.JobID(job.Id))
		require.NoError(t, err)
		require.Equal(t, livekit.JobStatus_JS_RUNNING, state2.Status)
	})
}

func TestWorkerAssignJob(t *testing.T) {
	t.Run("successful assignment", func(t *testing.T) {
		conn := newMockSignalConn()
		w := newTestWorker(conn)
		t.Cleanup(func() { w.Close() })

		job := makeTestJob()
		respondToAvailability(conn, w, true, false)

		state, err := w.AssignJob(context.Background(), job)
		require.NoError(t, err)
		require.NotNil(t, state)
		require.Equal(t, livekit.JobStatus_JS_RUNNING, state.Status)
		require.Equal(t, w.ID, state.WorkerId)
		require.NotZero(t, state.StartedAt)
		require.NotZero(t, state.UpdatedAt)
		require.Equal(t, "agent-identity", state.ParticipantIdentity)

		require.Equal(t, 1, w.RunningJobCount())

		msgs := conn.getMessages()
		var assignmentFound bool
		for _, msg := range msgs {
			if a, ok := msg.Message.(*livekit.ServerMessage_Assignment); ok {
				assignmentFound = true
				require.NotEmpty(t, a.Assignment.Token)
				require.Equal(t, job.Id, a.Assignment.Job.Id)
				break
			}
		}
		require.True(t, assignmentFound, "expected assignment message to be sent")
	})

	t.Run("duplicate job assignment", func(t *testing.T) {
		conn := newMockSignalConn()
		w := newTestWorker(conn)
		t.Cleanup(func() { w.Close() })

		job := makeTestJob()
		jobID := livekit.JobID(job.Id)

		w.mu.Lock()
		w.availability[jobID] = make(chan *livekit.AvailabilityResponse, 1)
		w.mu.Unlock()

		_, err := w.AssignJob(context.Background(), job)
		require.ErrorIs(t, err, ErrDuplicateJobAssignment)
	})

	t.Run("worker not available", func(t *testing.T) {
		conn := newMockSignalConn()
		w := newTestWorker(conn)
		t.Cleanup(func() { w.Close() })

		job := makeTestJob()
		respondToAvailability(conn, w, false, false)

		_, err := w.AssignJob(context.Background(), job)
		require.ErrorIs(t, err, ErrWorkerNotAvailable)

		require.Equal(t, 0, w.RunningJobCount())
	})

	t.Run("terminate response", func(t *testing.T) {
		conn := newMockSignalConn()
		w := newTestWorker(conn)
		t.Cleanup(func() { w.Close() })

		job := makeTestJob()
		respondToAvailability(conn, w, false, true)

		state, err := w.AssignJob(context.Background(), job)
		require.NoError(t, err)
		require.Equal(t, livekit.JobStatus_JS_SUCCESS, state.Status)
		require.NotZero(t, state.EndedAt)

		require.Equal(t, 0, w.RunningJobCount())
	})

	t.Run("worker closed", func(t *testing.T) {
		conn := newMockSignalConn()
		w := newTestWorker(conn)

		job := makeTestJob()

		go func() {
			time.Sleep(50 * time.Millisecond)
			w.Close()
		}()

		_, err := w.AssignJob(context.Background(), job)
		require.ErrorIs(t, err, ErrWorkerClosed)
	})

	t.Run("context cancelled", func(t *testing.T) {
		conn := newMockSignalConn()
		w := newTestWorker(conn)
		t.Cleanup(func() { w.Close() })

		job := makeTestJob()
		ctx, cancel := context.WithCancel(context.Background())

		go func() {
			time.Sleep(50 * time.Millisecond)
			cancel()
		}()

		_, err := w.AssignJob(ctx, job)
		require.Error(t, err)
	})

	t.Run("initializes nil state", func(t *testing.T) {
		conn := newMockSignalConn()
		w := newTestWorker(conn)
		t.Cleanup(func() { w.Close() })

		job := makeTestJob()
		require.Nil(t, job.State)

		respondToAvailability(conn, w, true, false)

		state, err := w.AssignJob(context.Background(), job)
		require.NoError(t, err)
		require.NotNil(t, state)
		require.Equal(t, livekit.JobStatus_JS_RUNNING, state.Status)
	})

	t.Run("does not mutate original job", func(t *testing.T) {
		conn := newMockSignalConn()
		w := newTestWorker(conn)
		t.Cleanup(func() { w.Close() })

		job := makeTestJob()
		originalID := job.Id
		respondToAvailability(conn, w, true, false)

		_, err := w.AssignJob(context.Background(), job)
		require.NoError(t, err)

		require.Equal(t, originalID, job.Id)
		require.Nil(t, job.State)
	})
}

func TestWorkerTerminateJob(t *testing.T) {
	t.Run("successful termination", func(t *testing.T) {
		conn := newMockSignalConn()
		w := newTestWorker(conn)
		t.Cleanup(func() { w.Close() })

		job := makeTestJobWithState()
		addRunningJob(w, job)

		state, err := w.TerminateJob(livekit.JobID(job.Id), rpc.JobTerminateReason(0))
		require.NoError(t, err)
		require.NotNil(t, state)
		require.Equal(t, livekit.JobStatus_JS_SUCCESS, state.Status)
		require.NotZero(t, state.EndedAt)

		require.Equal(t, 0, w.RunningJobCount())

		msgs := conn.getMessages()
		var terminationFound bool
		for _, msg := range msgs {
			if term, ok := msg.Message.(*livekit.ServerMessage_Termination); ok {
				terminationFound = true
				require.Equal(t, job.Id, term.Termination.JobId)
				break
			}
		}
		require.True(t, terminationFound, "expected termination message to be sent")
	})

	t.Run("agent left room results in failure", func(t *testing.T) {
		conn := newMockSignalConn()
		w := newTestWorker(conn)
		t.Cleanup(func() { w.Close() })

		job := makeTestJobWithState()
		addRunningJob(w, job)

		state, err := w.TerminateJob(livekit.JobID(job.Id), rpc.JobTerminateReason_AGENT_LEFT_ROOM)
		require.NoError(t, err)
		require.Equal(t, livekit.JobStatus_JS_FAILED, state.Status)
		require.Equal(t, "agent worker left the room", state.Error)
		require.NotZero(t, state.EndedAt)
	})

	t.Run("job not found", func(t *testing.T) {
		conn := newMockSignalConn()
		w := newTestWorker(conn)
		t.Cleanup(func() { w.Close() })

		_, err := w.TerminateJob("nonexistent", rpc.JobTerminateReason(0))
		require.ErrorIs(t, err, ErrJobNotFound)
	})
}

func TestWorkerUpdateJobStatus(t *testing.T) {
	t.Run("update status to running", func(t *testing.T) {
		conn := newMockSignalConn()
		w := newTestWorker(conn)
		t.Cleanup(func() { w.Close() })

		job := makeTestJobWithState()
		addRunningJob(w, job)

		state, err := w.UpdateJobStatus(&livekit.UpdateJobStatus{
			JobId:  job.Id,
			Status: livekit.JobStatus_JS_RUNNING,
		})
		require.NoError(t, err)
		require.Equal(t, livekit.JobStatus_JS_RUNNING, state.Status)
		require.NotZero(t, state.UpdatedAt)

		require.Equal(t, 1, w.RunningJobCount())
	})

	t.Run("ended status removes job", func(t *testing.T) {
		conn := newMockSignalConn()
		w := newTestWorker(conn)
		t.Cleanup(func() { w.Close() })

		job := makeTestJobWithState()
		addRunningJob(w, job)

		state, err := w.UpdateJobStatus(&livekit.UpdateJobStatus{
			JobId:  job.Id,
			Status: livekit.JobStatus_JS_SUCCESS,
		})
		require.NoError(t, err)
		require.Equal(t, livekit.JobStatus_JS_SUCCESS, state.Status)
		require.NotZero(t, state.EndedAt)

		require.Equal(t, 0, w.RunningJobCount())
	})

	t.Run("failed status removes job", func(t *testing.T) {
		conn := newMockSignalConn()
		w := newTestWorker(conn)
		t.Cleanup(func() { w.Close() })

		job := makeTestJobWithState()
		addRunningJob(w, job)

		state, err := w.UpdateJobStatus(&livekit.UpdateJobStatus{
			JobId:  job.Id,
			Status: livekit.JobStatus_JS_FAILED,
			Error:  "something went wrong",
		})
		require.NoError(t, err)
		require.Equal(t, livekit.JobStatus_JS_FAILED, state.Status)
		require.Equal(t, "something went wrong", state.Error)
		require.NotZero(t, state.EndedAt)

		require.Equal(t, 0, w.RunningJobCount())
	})

	t.Run("pending to running sets start time", func(t *testing.T) {
		conn := newMockSignalConn()
		w := newTestWorker(conn)
		t.Cleanup(func() { w.Close() })

		job := &livekit.Job{
			Id:        guid.New(guid.AgentJobPrefix),
			Type:      livekit.JobType_JT_ROOM,
			Room:      &livekit.Room{Name: "test-room"},
			AgentName: "test-agent",
			State: &livekit.JobState{
				Status:    livekit.JobStatus_JS_PENDING,
				StartedAt: 0,
			},
		}
		addRunningJob(w, job)

		state, err := w.UpdateJobStatus(&livekit.UpdateJobStatus{
			JobId:  job.Id,
			Status: livekit.JobStatus_JS_RUNNING,
		})
		require.NoError(t, err)
		require.Equal(t, livekit.JobStatus_JS_RUNNING, state.Status)
		require.NotZero(t, state.StartedAt)
	})

	t.Run("unknown job returns error", func(t *testing.T) {
		conn := newMockSignalConn()
		w := newTestWorker(conn)
		t.Cleanup(func() { w.Close() })

		_, err := w.UpdateJobStatus(&livekit.UpdateJobStatus{
			JobId:  "nonexistent",
			Status: livekit.JobStatus_JS_RUNNING,
		})
		require.Error(t, err)
	})

	t.Run("returns a clone of state", func(t *testing.T) {
		conn := newMockSignalConn()
		w := newTestWorker(conn)
		t.Cleanup(func() { w.Close() })

		job := makeTestJobWithState()
		addRunningJob(w, job)

		state, err := w.UpdateJobStatus(&livekit.UpdateJobStatus{
			JobId:  job.Id,
			Status: livekit.JobStatus_JS_RUNNING,
		})
		require.NoError(t, err)

		state.Error = "mutated"

		state2, err := w.GetJobState(livekit.JobID(job.Id))
		require.NoError(t, err)
		require.Empty(t, state2.Error)
	})
}

func TestWorkerHandleAvailability(t *testing.T) {
	t.Run("routes to waiting channel", func(t *testing.T) {
		conn := newMockSignalConn()
		w := newTestWorker(conn)
		t.Cleanup(func() { w.Close() })

		jobID := livekit.JobID("test-job-id")
		availCh := make(chan *livekit.AvailabilityResponse, 1)

		w.mu.Lock()
		w.availability[jobID] = availCh
		w.mu.Unlock()

		err := w.HandleAvailability(&livekit.AvailabilityResponse{
			JobId:     string(jobID),
			Available: true,
		})
		require.NoError(t, err)

		select {
		case res := <-availCh:
			require.True(t, res.Available)
			require.Equal(t, string(jobID), res.JobId)
		default:
			require.Fail(t, "expected response on availability channel")
		}

		w.mu.RLock()
		_, exists := w.availability[jobID]
		w.mu.RUnlock()
		require.False(t, exists)
	})

	t.Run("unknown job does not error", func(t *testing.T) {
		conn := newMockSignalConn()
		w := newTestWorker(conn)
		t.Cleanup(func() { w.Close() })

		err := w.HandleAvailability(&livekit.AvailabilityResponse{
			JobId:     "unknown-job",
			Available: true,
		})
		require.NoError(t, err)
	})
}

func TestWorkerHandleUpdateJob(t *testing.T) {
	t.Run("known job", func(t *testing.T) {
		conn := newMockSignalConn()
		w := newTestWorker(conn)
		t.Cleanup(func() { w.Close() })

		job := makeTestJobWithState()
		addRunningJob(w, job)

		err := w.HandleUpdateJob(&livekit.UpdateJobStatus{
			JobId:  job.Id,
			Status: livekit.JobStatus_JS_SUCCESS,
		})
		require.NoError(t, err)

		require.Equal(t, 0, w.RunningJobCount())
	})

	t.Run("unknown job returns nil error", func(t *testing.T) {
		conn := newMockSignalConn()
		w := newTestWorker(conn)
		t.Cleanup(func() { w.Close() })

		err := w.HandleUpdateJob(&livekit.UpdateJobStatus{
			JobId:  "nonexistent",
			Status: livekit.JobStatus_JS_SUCCESS,
		})
		require.NoError(t, err)
	})
}

func TestWorkerHandleUpdateWorker(t *testing.T) {
	conn := newMockSignalConn()
	w := newTestWorker(conn)
	t.Cleanup(func() { w.Close() })

	t.Run("updates status and load", func(t *testing.T) {
		status := livekit.WorkerStatus_WS_AVAILABLE
		err := w.HandleUpdateWorker(&livekit.UpdateWorkerStatus{
			Status: &status,
			Load:   0.5,
		})
		require.NoError(t, err)
		require.Equal(t, livekit.WorkerStatus_WS_AVAILABLE, w.Status())
		require.Equal(t, float32(0.5), w.Load())
	})

	t.Run("updates load without status change", func(t *testing.T) {
		err := w.HandleUpdateWorker(&livekit.UpdateWorkerStatus{
			Load: 0.9,
		})
		require.NoError(t, err)
		require.Equal(t, livekit.WorkerStatus_WS_AVAILABLE, w.Status())
		require.Equal(t, float32(0.9), w.Load())
	})

	t.Run("updates status to full", func(t *testing.T) {
		status := livekit.WorkerStatus_WS_FULL
		err := w.HandleUpdateWorker(&livekit.UpdateWorkerStatus{
			Status: &status,
			Load:   1.0,
		})
		require.NoError(t, err)
		require.Equal(t, livekit.WorkerStatus_WS_FULL, w.Status())
		require.Equal(t, float32(1.0), w.Load())
	})
}

func TestWorkerCloseAndIsClosed(t *testing.T) {
	t.Run("close", func(t *testing.T) {
		conn := newMockSignalConn()
		w := newTestWorker(conn)

		require.False(t, w.IsClosed())
		w.Close()
		require.True(t, w.IsClosed())
		require.True(t, conn.isClosed())
	})

	t.Run("double close is safe", func(t *testing.T) {
		conn := newMockSignalConn()
		w := newTestWorker(conn)

		w.Close()
		w.Close()
		require.True(t, w.IsClosed())
	})

	t.Run("getters work after close", func(t *testing.T) {
		conn := newMockSignalConn()
		w := newTestWorker(conn)

		job := makeTestJobWithState()
		addRunningJob(w, job)

		w.Close()

		_ = w.Status()
		_ = w.Load()
		_ = w.RunningJobs()
		_ = w.RunningJobCount()
	})
}

func TestWorkerHandleSimulateJob(t *testing.T) {
	t.Run("room job", func(t *testing.T) {
		conn := newMockSignalConn()
		w := newTestWorker(conn)
		t.Cleanup(func() { w.Close() })

		respondToAvailability(conn, w, false, false)

		err := w.HandleSimulateJob(&livekit.SimulateJobRequest{
			Room: &livekit.Room{Name: "test-room"},
		})
		require.NoError(t, err)

		require.Eventually(t, func() bool {
			msgs := conn.getMessages()
			for _, msg := range msgs {
				if _, ok := msg.Message.(*livekit.ServerMessage_Availability); ok {
					return true
				}
			}
			return false
		}, time.Second, 10*time.Millisecond)
	})

	t.Run("publisher job with participant", func(t *testing.T) {
		conn := newMockSignalConn()
		w := newTestWorker(conn)
		t.Cleanup(func() { w.Close() })

		respondToAvailability(conn, w, false, false)

		err := w.HandleSimulateJob(&livekit.SimulateJobRequest{
			Room:        &livekit.Room{Name: "test-room"},
			Participant: &livekit.ParticipantInfo{Identity: "test-participant"},
		})
		require.NoError(t, err)

		require.Eventually(t, func() bool {
			msgs := conn.getMessages()
			for _, msg := range msgs {
				if avail, ok := msg.Message.(*livekit.ServerMessage_Availability); ok {
					return avail.Availability.Job.Type == livekit.JobType_JT_PUBLISHER
				}
			}
			return false
		}, time.Second, 10*time.Millisecond)
	})
}

func TestWorkerConcurrentAccess(t *testing.T) {
	conn := newMockSignalConn()
	w := newTestWorker(conn)
	t.Cleanup(func() { w.Close() })

	for i := 0; i < 5; i++ {
		addRunningJob(w, makeTestJobWithState())
	}

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = w.Status()
			_ = w.Load()
			_ = w.RunningJobs()
			_ = w.RunningJobCount()
		}()
	}

	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			status := livekit.WorkerStatus_WS_AVAILABLE
			_ = w.HandleUpdateWorker(&livekit.UpdateWorkerStatus{
				Status: &status,
				Load:   0.5,
			})
		}()
	}

	wg.Wait()
}
