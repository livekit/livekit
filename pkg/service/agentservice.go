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
	"errors"
	"math/rand"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/livekit/livekit-server/pkg/agent"
	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/rtc"
	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/version"
	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/protocol/utils"
	"github.com/livekit/psrpc"
)

type AgentSocketUpgrader struct {
	websocket.Upgrader
}

func (u AgentSocketUpgrader) Upgrade(
	w http.ResponseWriter,
	r *http.Request,
	responseHeader http.Header,
) (
	conn *websocket.Conn,
	registration agent.WorkerRegistration,
	ok bool,
) {
	if u.CheckOrigin == nil {
		// allow connections from any origin, since script may be hosted anywhere
		// security is enforced by access tokens
		u.CheckOrigin = func(r *http.Request) bool {
			return true
		}
	}

	// reject non websocket requests
	if !websocket.IsWebSocketUpgrade(r) {
		w.WriteHeader(404)
		return
	}

	// require a claim
	claims := GetGrants(r.Context())
	if claims == nil || claims.Video == nil || !claims.Video.Agent {
		HandleError(w, r, http.StatusUnauthorized, rtc.ErrPermissionDenied)
		return
	}

	registration = agent.MakeWorkerRegistration()
	registration.ClientIP = GetClientIP(r)

	// upgrade
	conn, err := u.Upgrader.Upgrade(w, r, responseHeader)
	if err != nil {
		HandleError(w, r, http.StatusInternalServerError, err)
		return
	}

	if pv, err := strconv.Atoi(r.FormValue("protocol")); err == nil {
		registration.Protocol = agent.WorkerProtocolVersion(pv)
	}

	return conn, registration, true
}

func DispatchAgentWorkerSignal(c agent.SignalConn, h agent.WorkerSignalHandler, l logger.Logger) bool {
	req, _, err := c.ReadWorkerMessage()
	if err != nil {
		if IsWebSocketCloseError(err) {
			l.Debugw("worker closed WS connection", "wsError", err)
		} else {
			l.Errorw("error reading from websocket", err)
		}
		return false
	}

	if err := agent.DispatchWorkerSignal(req, h); err != nil {
		l.Warnw("unable to handle worker signal", err, "req", logger.Proto(req))
		return false
	}

	return true
}

func HandshakeAgentWorker(c agent.SignalConn, serverInfo *livekit.ServerInfo, registration agent.WorkerRegistration, l logger.Logger) (r agent.WorkerRegistration, ok bool) {
	wr := agent.NewWorkerRegisterer(c, serverInfo, registration)
	if err := c.SetReadDeadline(wr.Deadline()); err != nil {
		return
	}
	for !wr.Registered() {
		if ok = DispatchAgentWorkerSignal(c, wr, l); !ok {
			return
		}
	}
	if err := c.SetReadDeadline(time.Time{}); err != nil {
		return
	}
	return wr.Registration(), true
}

type AgentService struct {
	upgrader AgentSocketUpgrader

	*AgentHandler
}

type AgentHandler struct {
	agentServer rpc.AgentInternalServer
	mu          sync.Mutex
	logger      logger.Logger

	serverInfo  *livekit.ServerInfo
	workers     map[string]*agent.Worker
	jobToWorker map[livekit.JobID]*agent.Worker
	keyProvider auth.KeyProvider

	namespaceWorkers    map[workerKey][]*agent.Worker
	roomKeyCount        int
	publisherKeyCount   int
	participantKeyCount int
	namespaces          []string // namespaces deprecated
	agentNames          []string

	roomTopic        string
	publisherTopic   string
	participantTopic string
}

type workerKey struct {
	agentName string
	namespace string
	jobType   livekit.JobType
}

func NewAgentService(
	conf *config.Config,
	currentNode routing.LocalNode,
	bus psrpc.MessageBus,
	keyProvider auth.KeyProvider,
) (*AgentService, error) {
	s := &AgentService{}

	serverInfo := &livekit.ServerInfo{
		Edition:       livekit.ServerInfo_Standard,
		Version:       version.Version,
		Protocol:      types.CurrentProtocol,
		AgentProtocol: agent.CurrentProtocol,
		Region:        conf.Region,
		NodeId:        string(currentNode.NodeID()),
	}

	agentServer, err := rpc.NewAgentInternalServer(s, bus)
	if err != nil {
		return nil, err
	}
	s.AgentHandler = NewAgentHandler(
		agentServer,
		keyProvider,
		logger.GetLogger(),
		serverInfo,
		agent.RoomAgentTopic,
		agent.PublisherAgentTopic,
		agent.ParticipantAgentTopic,
	)
	return s, nil
}

func (s *AgentService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if conn, registration, ok := s.upgrader.Upgrade(w, r, nil); ok {
		s.HandleConnection(r.Context(), NewWSSignalConnection(conn), registration)
		conn.Close()
	}
}

func NewAgentHandler(
	agentServer rpc.AgentInternalServer,
	keyProvider auth.KeyProvider,
	logger logger.Logger,
	serverInfo *livekit.ServerInfo,
	roomTopic string,
	publisherTopic string,
	participantTopic string,
) *AgentHandler {
	return &AgentHandler{
		agentServer:      agentServer,
		logger:           logger.WithComponent("agents"),
		workers:          make(map[string]*agent.Worker),
		jobToWorker:      make(map[livekit.JobID]*agent.Worker),
		namespaceWorkers: make(map[workerKey][]*agent.Worker),
		serverInfo:       serverInfo,
		keyProvider:      keyProvider,
		roomTopic:        roomTopic,
		publisherTopic:   publisherTopic,
		participantTopic: participantTopic,
	}
}

func (h *AgentHandler) HandleConnection(ctx context.Context, conn agent.SignalConn, registration agent.WorkerRegistration) {
	registration, ok := HandshakeAgentWorker(conn, h.serverInfo, registration, h.logger)
	if !ok {
		return
	}

	apiKey := GetAPIKey(ctx)
	apiSecret := h.keyProvider.GetSecret(apiKey)

	worker := agent.NewWorker(registration, apiKey, apiSecret, conn, h.logger)
	h.registerWorker(worker)

	handlerWorker := &agentHandlerWorker{h, worker}
	for ok := true; ok; {
		ok = DispatchAgentWorkerSignal(conn, handlerWorker, worker.Logger())
	}

	h.deregisterWorker(worker)
	worker.Close()
}

func (h *AgentHandler) registerWorker(w *agent.Worker) {
	h.mu.Lock()

	h.workers[w.ID] = w

	key := workerKey{w.AgentName, w.Namespace, w.JobType}

	workers := h.namespaceWorkers[key]
	created := len(workers) == 0

	if created {
		nameTopic := agent.GetAgentTopic(w.AgentName, w.Namespace)
		var typeTopic string
		switch w.JobType {
		case livekit.JobType_JT_ROOM:
			typeTopic = h.roomTopic
		case livekit.JobType_JT_PUBLISHER:
			typeTopic = h.publisherTopic
		case livekit.JobType_JT_PARTICIPANT:
			typeTopic = h.participantTopic
		}

		err := h.agentServer.RegisterJobRequestTopic(nameTopic, typeTopic)
		if err != nil {
			h.mu.Unlock()

			w.Logger().Errorw("failed to register job request topic", err)
			w.Close()
			return
		}

		switch w.JobType {
		case livekit.JobType_JT_ROOM:
			h.roomKeyCount++
		case livekit.JobType_JT_PUBLISHER:
			h.publisherKeyCount++
		case livekit.JobType_JT_PARTICIPANT:
			h.participantKeyCount++
		}

		h.namespaces = append(h.namespaces, w.Namespace)
		sort.Strings(h.namespaces)
		h.agentNames = append(h.agentNames, w.AgentName)
		sort.Strings(h.agentNames)
	}

	h.namespaceWorkers[key] = append(workers, w)
	h.mu.Unlock()

	h.logger.Infow("worker registered",
		"namespace", w.Namespace,
		"jobType", w.JobType,
		"agentName", w.AgentName,
		"workerID", w.ID,
	)
	if created {
		err := h.agentServer.PublishWorkerRegistered(context.Background(), agent.DefaultHandlerNamespace, &emptypb.Empty{})
		// TODO: when this happens, should we disconnect the worker so it'll retry?
		if err != nil {
			w.Logger().Errorw("failed to publish worker registered", err, "namespace", w.Namespace, "jobType", w.JobType, "agentName", w.AgentName)
		}
	}
}

func (h *AgentHandler) deregisterWorker(w *agent.Worker) {
	h.mu.Lock()
	defer h.mu.Unlock()

	delete(h.workers, w.ID)

	key := workerKey{w.AgentName, w.Namespace, w.JobType}

	workers, ok := h.namespaceWorkers[key]
	if !ok {
		return
	}
	index := slices.Index(workers, w)
	if index == -1 {
		return
	}

	if len(workers) > 1 {
		h.namespaceWorkers[key] = slices.Delete(workers, index, index+1)
	} else {
		h.logger.Infow("last worker deregistered",
			"namespace", w.Namespace,
			"jobType", w.JobType,
			"agentName", w.AgentName,
			"workerID", w.ID,
		)
		delete(h.namespaceWorkers, key)

		topic := agent.GetAgentTopic(w.AgentName, w.Namespace)

		switch w.JobType {
		case livekit.JobType_JT_ROOM:
			h.roomKeyCount--
			h.agentServer.DeregisterJobRequestTopic(topic, h.roomTopic)
		case livekit.JobType_JT_PUBLISHER:
			h.publisherKeyCount--
			h.agentServer.DeregisterJobRequestTopic(topic, h.publisherTopic)
		case livekit.JobType_JT_PARTICIPANT:
			h.participantKeyCount--
			h.agentServer.DeregisterJobRequestTopic(topic, h.participantTopic)
		}

		// agentNames and namespaces contains repeated entries for each agentNames/namespaces combinations
		if i := slices.Index(h.namespaces, w.Namespace); i != -1 {
			h.namespaces = slices.Delete(h.namespaces, i, i+1)
		}
		if i := slices.Index(h.agentNames, w.AgentName); i != -1 {
			h.agentNames = slices.Delete(h.agentNames, i, i+1)
		}
	}

	jobs := w.RunningJobs()
	for jobID := range jobs {
		h.deregisterJob(jobID)
	}
}

func (h *AgentHandler) deregisterJob(jobID livekit.JobID) {
	h.agentServer.DeregisterJobTerminateTopic(string(jobID))

	delete(h.jobToWorker, jobID)

	// TODO update dispatch state
}

func (h *AgentHandler) JobRequest(ctx context.Context, job *livekit.Job) (*rpc.JobRequestResponse, error) {
	logger := h.logger.WithUnlikelyValues(
		"jobID", job.Id,
		"namespace", job.Namespace,
		"agentName", job.AgentName,
		"jobType", job.Type.String(),
	)
	if job.Room != nil {
		logger = logger.WithValues("room", job.Room.Name, "roomID", job.Room.Sid)
	}
	if job.Participant != nil {
		logger = logger.WithValues("participant", job.Participant.Identity)
	}

	key := workerKey{job.AgentName, job.Namespace, job.Type}
	attempted := make(map[*agent.Worker]struct{})
	for {
		selected, err := h.selectWorkerWeightedByLoad(key, attempted)
		if err != nil {
			logger.Warnw("no worker available to handle job", err)
			return nil, psrpc.NewError(psrpc.ResourceExhausted, err)
		}

		logger := logger.WithValues("workerID", selected.ID)
		attempted[selected] = struct{}{}

		state, err := selected.AssignJob(ctx, job)
		switch state.GetStatus() {
		case livekit.JobStatus_JS_RUNNING:
			logger.Infow("assigned job to worker")
			h.mu.Lock()
			h.jobToWorker[livekit.JobID(job.Id)] = selected
			h.mu.Unlock()

			err = h.agentServer.RegisterJobTerminateTopic(job.Id)
			if err != nil {
				logger.Errorw("failed to register JobTerminate handler", err)
			}
			fallthrough
		case livekit.JobStatus_JS_SUCCESS:
			return &rpc.JobRequestResponse{
				State: state,
			}, nil
		default:
			retry := utils.ErrorIsOneOf(err, agent.ErrWorkerNotAvailable, agent.ErrWorkerClosed)
			logger.Warnw("failed to assign job to worker", err, "retry", retry)
			if !retry {
				return nil, err
			}
		}
	}
}

func (h *AgentHandler) JobRequestAffinity(ctx context.Context, job *livekit.Job) float32 {
	h.mu.Lock()
	defer h.mu.Unlock()

	var affinity float32
	for _, w := range h.workers {
		if w.AgentName != job.AgentName || w.Namespace != job.Namespace || w.JobType != job.Type {
			continue
		}

		if w.Status() == livekit.WorkerStatus_WS_AVAILABLE {
			affinity += max(0, 1-w.Load())
		}
	}

	return affinity
}

func (h *AgentHandler) JobTerminate(ctx context.Context, req *rpc.JobTerminateRequest) (*rpc.JobTerminateResponse, error) {
	h.mu.Lock()
	w := h.jobToWorker[livekit.JobID(req.JobId)]
	h.mu.Unlock()

	if w == nil {
		return nil, psrpc.NewErrorf(psrpc.NotFound, "no worker for jobID")
	}

	state, err := w.TerminateJob(livekit.JobID(req.JobId), req.Reason)
	if err != nil {
		return nil, err
	}

	return &rpc.JobTerminateResponse{
		State: state,
	}, nil
}

func (h *AgentHandler) CheckEnabled(ctx context.Context, req *rpc.CheckEnabledRequest) (*rpc.CheckEnabledResponse, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	// This doesn't return the full agentName -> namespace mapping, which can cause some unnecessary RPC.
	// namespaces are however deprecated.
	return &rpc.CheckEnabledResponse{
		Namespaces:         slices.Compact(slices.Clone(h.namespaces)),
		AgentNames:         slices.Compact(slices.Clone(h.agentNames)),
		RoomEnabled:        h.roomKeyCount != 0,
		PublisherEnabled:   h.publisherKeyCount != 0,
		ParticipantEnabled: h.participantKeyCount != 0,
	}, nil
}

func (h *AgentHandler) DrainConnections(interval time.Duration) {
	// jitter drain start
	time.Sleep(time.Duration(rand.Int63n(int64(interval))))

	t := time.NewTicker(interval)
	defer t.Stop()

	h.mu.Lock()
	defer h.mu.Unlock()

	for _, w := range h.workers {
		w.Close()
		<-t.C
	}
}

func (h *AgentHandler) selectWorkerWeightedByLoad(key workerKey, ignore map[*agent.Worker]struct{}) (*agent.Worker, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	workers, ok := h.namespaceWorkers[key]
	if !ok {
		return nil, errors.New("no workers available")
	}

	normalizedLoads := make(map[*agent.Worker]float32)
	var availableSum float32
	for _, w := range workers {
		if _, ok := ignore[w]; !ok && w.Status() == livekit.WorkerStatus_WS_AVAILABLE {
			normalizedLoads[w] = max(0, 1-w.Load())
			availableSum += normalizedLoads[w]
		}
	}

	if availableSum == 0 {
		return nil, errors.New("no workers with sufficient capacity")
	}

	currentSum := rand.Float32() * availableSum
	for w, load := range normalizedLoads {
		if currentSum -= load; currentSum <= 0 {
			return w, nil
		}
	}
	return workers[0], nil
}

var _ agent.WorkerSignalHandler = (*agentHandlerWorker)(nil)

type agentHandlerWorker struct {
	h *AgentHandler
	*agent.Worker
}

func (w *agentHandlerWorker) HandleUpdateJob(update *livekit.UpdateJobStatus) error {
	if err := w.Worker.HandleUpdateJob(update); err != nil {
		return err
	}

	if agent.JobStatusIsEnded(update.Status) {
		w.h.mu.Lock()
		w.h.deregisterJob(livekit.JobID(update.JobId))
		w.h.mu.Unlock()
	}
	return nil
}
