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
	"golang.org/x/exp/maps"
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
	"github.com/livekit/psrpc"
)

type AgentSocketUpgrader struct {
	websocket.Upgrader
}

func (u AgentSocketUpgrader) Upgrade(w http.ResponseWriter, r *http.Request, responseHeader http.Header) (*websocket.Conn, agent.WorkerProtocolVersion, bool) {
	// reject non websocket requests
	if !websocket.IsWebSocketUpgrade(r) {
		w.WriteHeader(404)
		return nil, 0, false
	}

	// require a claim
	claims := GetGrants(r.Context())
	if claims == nil || claims.Video == nil || !claims.Video.Agent {
		handleError(w, r, http.StatusUnauthorized, rtc.ErrPermissionDenied)
		return nil, 0, false
	}

	// upgrade
	conn, err := u.Upgrader.Upgrade(w, r, responseHeader)
	if err != nil {
		handleError(w, r, http.StatusInternalServerError, err)
		return nil, 0, false
	}

	var protocol agent.WorkerProtocolVersion = agent.CurrentProtocol
	if pv, err := strconv.Atoi(r.FormValue("protocol")); err == nil {
		protocol = agent.WorkerProtocolVersion(pv)
	}

	return conn, protocol, true
}

type AgentService struct {
	upgrader AgentSocketUpgrader

	*AgentHandler
}

type AgentHandler struct {
	agent.UnimplementedWorkerHandler

	agentServer rpc.AgentInternalServer
	mu          sync.Mutex
	logger      logger.Logger

	serverInfo  *livekit.ServerInfo
	workers     map[string]*agent.Worker
	jobToWorker map[string]*agent.Worker
	keyProvider auth.KeyProvider

	namespaceWorkers  map[workerKey][]*agent.Worker
	roomKeyCount      int
	publisherKeyCount int
	namespaces        []string // namespaces deprecated
	agentNames        []string

	roomTopic      string
	publisherTopic string
}

type workerKey struct {
	agentName string
	namespace string
	jobType   livekit.JobType
}

func NewAgentService(conf *config.Config,
	currentNode routing.LocalNode,
	bus psrpc.MessageBus,
	keyProvider auth.KeyProvider,
) (*AgentService, error) {
	s := &AgentService{}

	// allow connections from any origin, since script may be hosted anywhere
	// security is enforced by access tokens
	s.upgrader.CheckOrigin = func(r *http.Request) bool {
		return true
	}

	serverInfo := &livekit.ServerInfo{
		Edition:       livekit.ServerInfo_Standard,
		Version:       version.Version,
		Protocol:      types.CurrentProtocol,
		AgentProtocol: agent.CurrentProtocol,
		Region:        conf.Region,
		NodeId:        currentNode.Id,
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
	)
	return s, nil
}

func (s *AgentService) ServeHTTP(writer http.ResponseWriter, r *http.Request) {
	if conn, protocol, ok := s.upgrader.Upgrade(writer, r, nil); ok {
		s.HandleConnection(r.Context(), NewWSSignalConnection(conn), protocol)
	}
}

func NewAgentHandler(
	agentServer rpc.AgentInternalServer,
	keyProvider auth.KeyProvider,
	logger logger.Logger,
	serverInfo *livekit.ServerInfo,
	roomTopic string,
	publisherTopic string,
) *AgentHandler {
	return &AgentHandler{
		agentServer:      agentServer,
		logger:           logger,
		workers:          make(map[string]*agent.Worker),
		jobToWorker:      make(map[string]*agent.Worker),
		namespaceWorkers: make(map[workerKey][]*agent.Worker),
		serverInfo:       serverInfo,
		keyProvider:      keyProvider,
		roomTopic:        roomTopic,
		publisherTopic:   publisherTopic,
	}
}

func (h *AgentHandler) InsertWorker(w *agent.Worker) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.workers[w.ID()] = w
}

func (h *AgentHandler) DeleteWorker(w *agent.Worker) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.workers, w.ID())
}

func (h *AgentHandler) Workers() []*agent.Worker {
	h.mu.Lock()
	defer h.mu.Unlock()
	return maps.Values(h.workers)
}

func (h *AgentHandler) HandleConnection(ctx context.Context, conn agent.SignalConn, protocol agent.WorkerProtocolVersion) {
	apiKey := GetAPIKey(ctx)
	apiSecret := h.keyProvider.GetSecret(apiKey)

	worker := agent.NewWorker(protocol, apiKey, apiSecret, h.serverInfo, conn, h.logger, h)

	h.InsertWorker(worker)

	for {
		req, _, err := conn.ReadWorkerMessage()
		if err != nil {
			if IsWebSocketCloseError(err) {
				worker.Logger().Infow("worker closed WS connection", "wsError", err)
			} else {
				worker.Logger().Errorw("error reading from websocket", err)
			}
			break
		}

		worker.HandleMessage(req)
	}

	h.DeleteWorker(worker)

	worker.Close()
}

func (h *AgentHandler) HandleWorkerRegister(w *agent.Worker) {
	h.mu.Lock()

	key := workerKey{w.AgentName(), w.Namespace(), w.JobType()}

	workers := h.namespaceWorkers[key]
	created := len(workers) == 0

	if created {
		nameTopic := agent.GetAgentTopic(w.AgentName(), w.Namespace())
		typeTopic := h.roomTopic
		if w.JobType() == livekit.JobType_JT_PUBLISHER {
			typeTopic = h.publisherTopic
		}
		err := h.agentServer.RegisterJobRequestTopic(nameTopic, typeTopic)
		if err != nil {
			h.mu.Unlock()

			w.Logger().Errorw("failed to register job request topic", err)
			w.Close()
			return
		}

		if w.JobType() == livekit.JobType_JT_ROOM {
			h.roomKeyCount++
		} else {
			h.publisherKeyCount++
		}

		h.namespaces = append(h.namespaces, w.Namespace())
		sort.Strings(h.namespaces)
		h.agentNames = append(h.agentNames, w.AgentName())
		sort.Strings(h.agentNames)

	}

	h.namespaceWorkers[key] = append(workers, w)
	h.mu.Unlock()

	if created {
		h.logger.Infow("initial worker registered", "namespace", w.Namespace(), "jobType", w.JobType(), "agentName", w.AgentName())
		err := h.agentServer.PublishWorkerRegistered(context.Background(), agent.DefaultHandlerNamespace, &emptypb.Empty{})
		if err != nil {
			w.Logger().Errorw("failed to publish worker registered", err, "namespace", w.Namespace(), "jobType", w.JobType(), "agentName", w.AgentName())
		}
	}
}

func (h *AgentHandler) HandleWorkerDeregister(w *agent.Worker) {
	h.mu.Lock()
	defer h.mu.Unlock()

	key := workerKey{w.AgentName(), w.Namespace(), w.JobType()}

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
		h.logger.Debugw("last worker deregistered", "namespace", w.Namespace(), "jobType", w.JobType(), "agentName", w.AgentName())
		delete(h.namespaceWorkers, key)

		topic := agent.GetAgentTopic(w.AgentName(), w.Namespace())
		if w.JobType() == livekit.JobType_JT_ROOM {
			h.roomKeyCount--
			h.agentServer.DeregisterJobRequestTopic(topic, h.roomTopic)
		} else {
			h.publisherKeyCount--
			h.agentServer.DeregisterJobRequestTopic(topic, h.publisherTopic)
		}

		// agentNames and namespaces contains repeated entries for each agentNames/namespaces combinations
		if i := slices.Index(h.namespaces, w.Namespace()); i != -1 {
			h.namespaces = slices.Delete(h.namespaces, i, i+1)
		}
		if i := slices.Index(h.agentNames, w.AgentName()); i != -1 {
			h.agentNames = slices.Delete(h.agentNames, i, i+1)
		}
	}

	jobs := w.RunningJobs()
	for _, j := range jobs {
		h.deregisterJob(j.Id)
	}
}

func (h *AgentHandler) HandleWorkerJobStatus(w *agent.Worker, status *livekit.UpdateJobStatus) {
	if agent.JobStatusIsEnded(status.Status) {
		h.deregisterJob(status.JobId)
	}
}

func (h *AgentHandler) deregisterJob(jobID string) {
	h.agentServer.DeregisterJobTerminateTopic(jobID)

	h.mu.Lock()
	delete(h.jobToWorker, jobID)
	h.mu.Unlock()

	// TODO update dispatch state
}

func (h *AgentHandler) JobRequest(ctx context.Context, job *livekit.Job) (*rpc.JobRequestResponse, error) {
	key := workerKey{job.AgentName, job.Namespace, job.Type}
	attempted := make(map[*agent.Worker]struct{})
	for {
		selected, err := h.selectWorkerWeightedByLoad(key, attempted)
		if err != nil {
			return nil, psrpc.NewError(psrpc.DeadlineExceeded, err)
		}

		attempted[selected] = struct{}{}

		values := []interface{}{
			"jobID", job.Id,
			"namespace", job.Namespace,
			"agentName", job.AgentName,
			"workerID", selected.ID(),
		}
		if job.Room != nil {
			values = append(values, "room", job.Room.Name, "roomID", job.Room.Sid)
		}
		if job.Participant != nil {
			values = append(values, "participant", job.Participant.Identity)
		}
		h.logger.Debugw("assigning job", values...)
		err = selected.AssignJob(ctx, job)
		if err != nil {
			if errors.Is(err, agent.ErrWorkerNotAvailable) {
				continue // Try another worker
			}
			return nil, err
		}
		h.mu.Lock()
		h.jobToWorker[job.Id] = selected
		h.mu.Unlock()

		err = h.agentServer.RegisterJobTerminateTopic(job.Id)
		if err != nil {
			h.logger.Errorw("failes registering JobTerminate handler", err, values...)
		}

		return &rpc.JobRequestResponse{
			State: job.State,
		}, nil
	}
}

func (h *AgentHandler) JobRequestAffinity(ctx context.Context, job *livekit.Job) float32 {
	h.mu.Lock()
	defer h.mu.Unlock()

	var affinity float32
	var maxLoad float32
	for _, w := range h.workers {
		if w.AgentName() != job.AgentName || w.Namespace() != job.Namespace || w.JobType() != job.Type {
			continue
		}

		if w.Status() == livekit.WorkerStatus_WS_AVAILABLE {
			load := w.Load()
			if len(w.RunningJobs()) > 0 && load > maxLoad {
				maxLoad = load
				affinity = 0.5 + load/2
			} else if affinity == 0 {
				affinity = 0.5
			}
		}
	}

	return affinity
}

func (h *AgentHandler) JobTerminate(ctx context.Context, req *rpc.JobTerminateRequest) (*rpc.JobTerminateResponse, error) {
	h.mu.Lock()
	w := h.jobToWorker[req.JobId]
	h.mu.Unlock()

	if w == nil {
		return nil, psrpc.NewErrorf(psrpc.NotFound, "no worker for jobID")
	}

	state, err := w.TerminateJob(req.JobId, req.Reason)
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
		Namespaces:       slices.Compact(slices.Clone(h.namespaces)),
		AgentNames:       slices.Compact(slices.Clone(h.agentNames)),
		RoomEnabled:      h.roomKeyCount != 0,
		PublisherEnabled: h.publisherKeyCount != 0,
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

	normalizeLoad := func(load float32) int {
		if load >= 1 {
			return 0
		}
		return int((1 - load) * 100)
	}

	normalizedLoads := make(map[*agent.Worker]int)
	var availableSum int
	for _, w := range workers {
		if _, ok := ignore[w]; !ok && w.Status() == livekit.WorkerStatus_WS_AVAILABLE {
			normalizedLoads[w] = normalizeLoad(w.Load())
			availableSum += normalizedLoads[w]
		}
	}

	if availableSum == 0 {
		return nil, errors.New("no workers with sufficient capacity")
	}

	threshold := rand.Intn(availableSum)
	var currentSum int
	for w, load := range normalizedLoads {
		currentSum += load
		if currentSum >= threshold {
			return w, nil
		}
	}

	return nil, errors.New("no workers available")
}
