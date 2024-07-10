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

package agent

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	pagent "github.com/livekit/protocol/agent"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils/guid"
)

type WorkerProtocolVersion int

const CurrentProtocol = 1

const (
	registerTimeout  = 10 * time.Second
	assignJobTimeout = 10 * time.Second
	pingFrequency    = 10 * time.Second
)

var (
	ErrWorkerClosed           = errors.New("worker closed")
	ErrWorkerNotAvailable     = errors.New("worker not available")
	ErrAvailabilityTimeout    = errors.New("agent worker availability timeout")
	ErrDuplicateJobAssignment = errors.New("duplicate job assignment")
)

type SignalConn interface {
	WriteServerMessage(msg *livekit.ServerMessage) (int, error)
	ReadWorkerMessage() (*livekit.WorkerMessage, int, error)
	Close() error
}

type WorkerHandler interface {
	HandleWorkerRegister(w *Worker)
	HandleWorkerDeregister(w *Worker)
	HandleWorkerStatus(w *Worker, status *livekit.UpdateWorkerStatus)
	HandleWorkerJobStatus(w *Worker, status *livekit.UpdateJobStatus)
	HandleWorkerSimulateJob(w *Worker, job *livekit.Job)
	HandleWorkerMigrateJob(w *Worker, request *livekit.MigrateJobRequest)
}

var _ WorkerHandler = UnimplementedWorkerHandler{}

type UnimplementedWorkerHandler struct{}

func (UnimplementedWorkerHandler) HandleWorkerRegister(*Worker)                               {}
func (UnimplementedWorkerHandler) HandleWorkerDeregister(*Worker)                             {}
func (UnimplementedWorkerHandler) HandleWorkerStatus(*Worker, *livekit.UpdateWorkerStatus)    {}
func (UnimplementedWorkerHandler) HandleWorkerJobStatus(*Worker, *livekit.UpdateJobStatus)    {}
func (UnimplementedWorkerHandler) HandleWorkerSimulateJob(*Worker, *livekit.Job)              {}
func (UnimplementedWorkerHandler) HandleWorkerMigrateJob(*Worker, *livekit.MigrateJobRequest) {}

func JobStatusIsEnded(s livekit.JobStatus) bool {
	return s == livekit.JobStatus_JS_SUCCESS || s == livekit.JobStatus_JS_FAILED
}

type Worker struct {
	id          string
	jobType     livekit.JobType
	version     string
	agentName   string
	name        string
	namespace   string
	load        float32
	permissions *livekit.ParticipantPermission
	apiKey      string
	apiSecret   string
	serverInfo  *livekit.ServerInfo
	mu          sync.Mutex

	protocolVersion WorkerProtocolVersion
	registered      atomic.Bool
	status          livekit.WorkerStatus
	runningJobs     map[string]*livekit.Job

	handler WorkerHandler

	conn   SignalConn
	closed chan struct{}

	availability map[string]chan *livekit.AvailabilityResponse

	ctx    context.Context
	cancel context.CancelFunc

	logger logger.Logger
}

func NewWorker(
	protocolVersion WorkerProtocolVersion,
	apiKey string,
	apiSecret string,
	serverInfo *livekit.ServerInfo,
	conn SignalConn,
	logger logger.Logger,
	handler WorkerHandler,
) *Worker {
	ctx, cancel := context.WithCancel(context.Background())
	id := guid.New(guid.AgentWorkerPrefix)

	w := &Worker{
		id:              id,
		protocolVersion: protocolVersion,
		apiKey:          apiKey,
		apiSecret:       apiSecret,
		serverInfo:      serverInfo,
		closed:          make(chan struct{}),
		runningJobs:     make(map[string]*livekit.Job),
		availability:    make(map[string]chan *livekit.AvailabilityResponse),
		conn:            conn,
		ctx:             ctx,
		cancel:          cancel,
		logger:          logger.WithValues("workerID", id),
		handler:         handler,
	}

	time.AfterFunc(registerTimeout, func() {
		if !w.registered.Load() && !w.IsClosed() {
			w.logger.Warnw("worker did not register in time", nil, "id", w.id)
			w.Close()
		}
	})

	return w
}

func (w *Worker) sendRequest(req *livekit.ServerMessage) {
	if _, err := w.conn.WriteServerMessage(req); err != nil {
		w.logger.Errorw("error writing to websocket", err)
	}
}

func (w *Worker) ID() string {
	return w.id
}

func (w *Worker) JobType() livekit.JobType {
	return w.jobType
}

func (w *Worker) Namespace() string {
	return w.namespace
}

func (w *Worker) AgentName() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.agentName
}

func (w *Worker) Status() livekit.WorkerStatus {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.status
}

func (w *Worker) Load() float32 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.load
}

func (w *Worker) Logger() logger.Logger {
	return w.logger
}

func (w *Worker) RunningJobs() map[string]*livekit.Job {
	w.mu.Lock()
	defer w.mu.Unlock()
	jobs := make(map[string]*livekit.Job, len(w.runningJobs))
	for k, v := range w.runningJobs {
		jobs[k] = v
	}
	return jobs
}

func (w *Worker) AssignJob(ctx context.Context, job *livekit.Job) error {
	availCh := make(chan *livekit.AvailabilityResponse, 1)

	w.mu.Lock()
	if _, ok := w.availability[job.Id]; ok {
		w.mu.Unlock()
		return ErrDuplicateJobAssignment
	}

	w.availability[job.Id] = availCh
	w.mu.Unlock()

	defer func() {
		w.mu.Lock()
		delete(w.availability, job.Id)
		w.mu.Unlock()
	}()

	now := time.Now()
	if job.State == nil {
		job.State = &livekit.JobState{
			UpdatedAt: now.UnixNano(),
		}
	}

	w.sendRequest(&livekit.ServerMessage{Message: &livekit.ServerMessage_Availability{
		Availability: &livekit.AvailabilityRequest{Job: job},
	}})

	timeout := time.NewTimer(assignJobTimeout)
	defer timeout.Stop()

	// See handleAvailability for the response
	select {
	case res := <-availCh:
		if !res.Available {
			return ErrWorkerNotAvailable
		}

		token, err := pagent.BuildAgentToken(w.apiKey, w.apiSecret, job.Room.Name, res.ParticipantIdentity, res.ParticipantName, res.ParticipantMetadata, w.permissions)
		if err != nil {
			w.logger.Errorw("failed to build agent token", err)
			return err
		}

		// In OSS, Url is nil, and the used API Key is the same as the one used to connect the worker
		w.sendRequest(&livekit.ServerMessage{Message: &livekit.ServerMessage_Assignment{
			Assignment: &livekit.JobAssignment{Job: job, Url: nil, Token: token},
		}})

		w.mu.Lock()
		w.runningJobs[job.Id] = job
		w.mu.Unlock()

		// TODO sweep jobs that are never started. We can't do this until all SDKs actually update the the JOB state

		return nil
	case <-timeout.C:
		return ErrAvailabilityTimeout
	case <-w.ctx.Done():
		return ErrWorkerClosed
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (w *Worker) UpdateMetadata(metadata string) {
	w.logger.Debugw("worker metadata updated", nil, "metadata", metadata)
}

func (w *Worker) IsClosed() bool {
	select {
	case <-w.closed:
		return true
	default:
		return false
	}
}

func (w *Worker) Close() {
	w.mu.Lock()
	if w.IsClosed() {
		w.mu.Unlock()
		return
	}

	w.logger.Infow("closing worker")

	close(w.closed)
	w.cancel()
	_ = w.conn.Close()
	w.mu.Unlock()

	if w.registered.Load() {
		w.handler.HandleWorkerDeregister(w)
	}
}

func (w *Worker) HandleMessage(req *livekit.WorkerMessage) {
	switch m := req.Message.(type) {
	case *livekit.WorkerMessage_Register:
		w.handleRegister(m.Register)
	case *livekit.WorkerMessage_Availability:
		w.handleAvailability(m.Availability)
	case *livekit.WorkerMessage_UpdateJob:
		w.handleJobUpdate(m.UpdateJob)
	case *livekit.WorkerMessage_SimulateJob:
		w.handleSimulateJob(m.SimulateJob)
	case *livekit.WorkerMessage_Ping:
		w.handleWorkerPing(m.Ping)
	case *livekit.WorkerMessage_UpdateWorker:
		w.handleWorkerStatus(m.UpdateWorker)
	case *livekit.WorkerMessage_MigrateJob:
		w.handleMigrateJob(m.MigrateJob)
	}
}

func (w *Worker) handleRegister(req *livekit.RegisterWorkerRequest) {
	w.mu.Lock()
	var err error
	if w.IsClosed() {
		err = errors.New("worker closed")
	}
	if w.registered.Swap(true) {
		err = errors.New("worker already registered")
	}
	if err != nil {
		w.mu.Unlock()
		w.logger.Warnw("unable to register worker", err, "id", w.id)
		return
	}

	w.version = req.Version
	w.name = req.Name
	w.agentName = req.GetAgentName()
	w.namespace = req.GetNamespace()
	w.jobType = req.GetType()

	if req.AllowedPermissions != nil {
		w.permissions = req.AllowedPermissions
	} else {
		// Use default agent permissions
		w.permissions = &livekit.ParticipantPermission{
			CanSubscribe:      true,
			CanPublish:        true,
			CanPublishData:    true,
			CanUpdateMetadata: true,
		}
	}

	w.status = livekit.WorkerStatus_WS_AVAILABLE
	w.mu.Unlock()

	w.logger.Debugw("worker registered", "request", logger.Proto(req))

	w.sendRequest(&livekit.ServerMessage{
		Message: &livekit.ServerMessage_Register{
			Register: &livekit.RegisterWorkerResponse{
				WorkerId:   w.ID(),
				ServerInfo: w.serverInfo,
			},
		},
	})

	w.handler.HandleWorkerRegister(w)
}

func (w *Worker) handleAvailability(res *livekit.AvailabilityResponse) {
	w.mu.Lock()
	defer w.mu.Unlock()

	availCh, ok := w.availability[res.JobId]
	if !ok {
		w.logger.Warnw("received availability response for unknown job", nil, "jobId", res.JobId)
		return
	}

	availCh <- res
	delete(w.availability, res.JobId)
}

func (w *Worker) handleJobUpdate(update *livekit.UpdateJobStatus) {
	w.mu.Lock()

	job, ok := w.runningJobs[update.JobId]
	if !ok {
		w.logger.Infow("received job update for unknown job", "jobId", update.JobId)
		return
	}

	now := time.Now()
	job.State.UpdatedAt = now.UnixNano()

	if job.State.Status == livekit.JobStatus_JS_PENDING && JobStatusIsEnded(update.Status) {
		job.State.StartedAt = now.UnixNano()
	}

	if job.State.Status < livekit.JobStatus_JS_SUCCESS && JobStatusIsEnded(update.Status) {
		job.State.EndedAt = now.UnixNano()
	}

	job.State.Status = update.Status
	job.State.Error = update.Error

	// TODO do not delete, leave inside the JobDefinition
	if JobStatusIsEnded(job.State.Status) {
		delete(w.runningJobs, job.Id)
	}
	w.mu.Unlock()

	w.handler.HandleWorkerJobStatus(w, update)
}

func (w *Worker) handleSimulateJob(simulate *livekit.SimulateJobRequest) {
	jobType := livekit.JobType_JT_ROOM
	if simulate.Participant != nil {
		jobType = livekit.JobType_JT_PUBLISHER
	}

	job := &livekit.Job{
		Id:          guid.New(guid.AgentJobPrefix),
		Type:        jobType,
		Room:        simulate.Room,
		Participant: simulate.Participant,
		Namespace:   w.Namespace(),
		AgentName:   w.AgentName(),
	}

	go func() {
		err := w.AssignJob(w.ctx, job)
		if err != nil {
			w.logger.Errorw("failed to simulate job, assignment failed", err, "jobId", job.Id)
		} else {
			w.handler.HandleWorkerSimulateJob(w, job)
		}
	}()
}

func (w *Worker) handleWorkerPing(ping *livekit.WorkerPing) {
	w.sendRequest(&livekit.ServerMessage{Message: &livekit.ServerMessage_Pong{
		Pong: &livekit.WorkerPong{
			LastTimestamp: ping.Timestamp,
			Timestamp:     time.Now().UnixMilli(),
		},
	}})
}

func (w *Worker) handleWorkerStatus(update *livekit.UpdateWorkerStatus) {
	w.logger.Debugw("worker status update", "update", logger.Proto(update))

	w.mu.Lock()
	if update.Status != nil {
		w.status = update.GetStatus()
	}
	w.load = update.GetLoad()
	w.mu.Unlock()

	w.handler.HandleWorkerStatus(w, update)
}

func (w *Worker) handleMigrateJob(migrate *livekit.MigrateJobRequest) {
	// TODO(theomonnom): On OSS this is not implemented
	// We could maybe just move a specific job to another worker
	w.handler.HandleWorkerMigrateJob(w, migrate)
}
