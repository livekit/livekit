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
	"github.com/livekit/protocol/utils"
	putil "github.com/livekit/protocol/utils"
)

type WorkerProtocolVersion int

const CurrentProtocol = 1

const (
	registerTimeout  = 10 * time.Second
	assignJobTimeout = 10 * time.Second
	pingFrequency    = 10 * time.Second
)

var (
	ErrWorkerClosed        = errors.New("worker closed")
	ErrWorkerNotAvailable  = errors.New("worker not available")
	ErrAvailabilityTimeout = errors.New("agent worker availability timeout")
)

type Worker struct {
	id          string
	jobType     livekit.JobType
	version     string
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
	runningJobs     map[string]*Job

	onWorkerRegistered func(w *Worker)

	msgChan chan *livekit.ServerMessage
	closed  chan struct{}

	availibility map[string]chan *livekit.AvailabilityResponse

	ctx    context.Context
	cancel context.CancelFunc

	Logger logger.Logger
}

func NewWorker(
	protocolVersion WorkerProtocolVersion,
	apiKey string,
	apiSecret string,
	serverInfo *livekit.ServerInfo,
	logger logger.Logger,
) *Worker {
	ctx, cancel := context.WithCancel(context.Background())

	w := &Worker{
		id:              putil.NewGuid(utils.AgentWorkerPrefix),
		protocolVersion: protocolVersion,
		apiKey:          apiKey,
		apiSecret:       apiSecret,
		serverInfo:      serverInfo,
		closed:          make(chan struct{}),
		runningJobs:     make(map[string]*Job),
		msgChan:         make(chan *livekit.ServerMessage),
		ctx:             ctx,
		cancel:          cancel,
		Logger:          logger,
	}

	go func() {
		<-time.After(registerTimeout)
		if !w.registered.Load() && !w.IsClosed() {
			w.Logger.Warnw("worker did not register in time", nil, "id", w.id)
			w.Close()
		}
	}()

	return w
}

func (w *Worker) sendRequest(req *livekit.ServerMessage) {
	w.msgChan <- req
}

func (w *Worker) ReadChan() <-chan *livekit.ServerMessage {
	return w.msgChan
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

func (w *Worker) OnWorkerRegistered(f func(w *Worker)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.onWorkerRegistered = f
}

func (w *Worker) Registered() bool {
	return w.registered.Load()
}

func (w *Worker) RunningJobs() map[string]*Job {
	jobs := make(map[string]*Job, len(w.runningJobs))
	w.mu.Lock()
	defer w.mu.Unlock()
	for k, v := range w.runningJobs {
		jobs[k] = v
	}
	return jobs
}

func (w *Worker) AssignJob(ctx context.Context, job *livekit.Job) error {
	availCh := make(chan *livekit.AvailabilityResponse, 1)

	w.mu.Lock()
	w.availibility[job.Id] = availCh
	w.mu.Unlock()

	w.sendRequest(&livekit.ServerMessage{Message: &livekit.ServerMessage_Availability{
		Availability: &livekit.AvailabilityRequest{Job: job},
	}})

	// See handleAvailability for the response
	select {
	case res := <-availCh:
		if !res.Available {
			return ErrWorkerNotAvailable
		}

		token, err := pagent.BuildAgentToken(w.apiKey, w.apiSecret, job.Room.Name, res.ParticipantIdentity, res.ParticipantName, res.ParticipantMetadata, w.permissions)
		if err != nil {
			w.Logger.Errorw("failed to build agent token", err)
			return err
		}

		// In OSS, Url is nil, and the used token is the same as the one used to connect the worker
		w.sendRequest(&livekit.ServerMessage{Message: &livekit.ServerMessage_Assignment{
			Assignment: &livekit.JobAssignment{Job: job, Url: nil, Token: token},
		}})

		// TODO(theomonnom): Check if an agent was successfully connected to the room before returning
		return nil
	case <-time.After(assignJobTimeout):
		return ErrAvailabilityTimeout
	case <-w.ctx.Done():
		return ErrWorkerClosed
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (w *Worker) UpdateStatus(status *livekit.UpdateWorkerStatus) {
	w.mu.Lock()
	if status.Status != nil {
		w.status = status.GetStatus()
	}
	w.load = status.GetLoad()
	w.mu.Unlock()

	if status.Metadata != nil {
		w.UpdateMetadata(status.GetMetadata())
	}
}

func (w *Worker) UpdateMetadata(metadata string) {
	w.Logger.Debugw("worker metadata updated", nil, "metadata", metadata)
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

	w.Logger.Infow("closing worker")

	close(w.closed)
	w.cancel()
	close(w.msgChan)
	w.mu.Unlock()
}

func (w *Worker) HandleMessage(req *livekit.WorkerMessage) {
	switch m := req.Message.(type) {
	case *livekit.WorkerMessage_Register:
		go w.handleRegister(m.Register)
	case *livekit.WorkerMessage_Availability:
		go w.handleAvailability(m.Availability)
	case *livekit.WorkerMessage_UpdateJob:
		go w.handleJobUpdate(m.UpdateJob)
	case *livekit.WorkerMessage_SimulateJob:
		go w.handleSimulateJob(m.SimulateJob)
	case *livekit.WorkerMessage_Ping:
		go w.handleWorkerPing(m.Ping)
	case *livekit.WorkerMessage_UpdateWorker:
		go w.handleWorkerStatus(m.UpdateWorker)
	case *livekit.WorkerMessage_MigrateJob:
		go w.handleMigrateJob(m.MigrateJob)
	}
}

func (w *Worker) handleRegister(req *livekit.RegisterWorkerRequest) {
	if w.registered.Load() {
		w.Logger.Warnw("worker already registered", nil, "id", w.id)
		return
	}

	w.mu.Lock()
	onWorkerRegistered := w.onWorkerRegistered
	w.jobType = req.Type
	w.version = req.Version
	w.name = req.Name
	w.permissions = req.AllowedPermissions
	w.registered.Store(true)
	w.mu.Unlock()

	w.sendRequest(&livekit.ServerMessage{
		Message: &livekit.ServerMessage_Register{
			Register: &livekit.RegisterWorkerResponse{
				WorkerId:   w.ID(),
				ServerInfo: w.serverInfo,
			},
		},
	})

	if onWorkerRegistered != nil {
		onWorkerRegistered(w)
	}
}

func (w *Worker) handleAvailability(res *livekit.AvailabilityResponse) {
	w.mu.Lock()
	defer w.mu.Unlock()

	availCh, ok := w.availibility[res.JobId]
	if !ok {
		w.Logger.Warnw("received availability response for unknown job", nil, "jobId", res.JobId)
		return
	}

	availCh <- res
	delete(w.availibility, res.JobId)
}

func (w *Worker) handleJobUpdate(update *livekit.UpdateJobStatus) {
	w.mu.Lock()
	job, ok := w.runningJobs[update.JobId]
	w.mu.Unlock()

	if !ok {
		w.Logger.Warnw("received job update for unknown job", nil, "jobId", update.JobId)
		return
	}

	job.UpdateStatus(update)
}

func (w *Worker) handleSimulateJob(simulate *livekit.SimulateJobRequest) {
	jobType := livekit.JobType_JT_ROOM
	if simulate.Participant != nil {
		jobType = livekit.JobType_JT_PUBLISHER
	}

	job := &livekit.Job{
		Id:          utils.NewGuid(utils.AgentJobPrefix),
		Type:        jobType,
		Room:        simulate.Room,
		Participant: simulate.Participant,
		Namespace:   w.Namespace(),
	}

	ctx := context.Background()
	err := w.AssignJob(ctx, job)
	if err != nil {
		w.Logger.Errorw("failed to simulate job, assignment failed", err, "jobId", job.Id)
	}

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
	w.UpdateStatus(update)
}

func (w *Worker) handleMigrateJob(migrate *livekit.MigrateJobRequest) {
	// TODO(theomonnom): On OSS this is not implemented
	// We could maybe just move a specfic job to another worker
}
