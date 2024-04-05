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
	"io"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
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
	"github.com/livekit/psrpc"
)

type AgentService struct {
	upgrader websocket.Upgrader

	*AgentHandler
}

type AgentHandler struct {
	agentServer rpc.AgentInternalServer
	mu          sync.Mutex
	logger      logger.Logger

	serverInfo  *livekit.ServerInfo
	workers     map[string]*agent.Worker
	keyProvider auth.KeyProvider

	namespaces       map[string]*namespaceInfo
	publisherEnabled bool
	roomEnabled      bool
	roomTopic        string
	publisherTopic   string
}

type namespaceInfo struct {
	numPublishers int32
	numRooms      int32
}

func NewAgentService(conf *config.Config,
	currentNode routing.LocalNode,
	bus psrpc.MessageBus,
	keyProvider auth.KeyProvider,
) (*AgentService, error) {
	s := &AgentService{
		upgrader: websocket.Upgrader{},
	}

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
	// reject non websocket requests
	if !websocket.IsWebSocketUpgrade(r) {
		writer.WriteHeader(404)
		return
	}

	// require a claim
	claims := GetGrants(r.Context())
	if claims == nil || claims.Video == nil || !claims.Video.Agent {
		handleError(writer, r, http.StatusUnauthorized, rtc.ErrPermissionDenied)
		return
	}

	// upgrade
	conn, err := s.upgrader.Upgrade(writer, r, nil)
	if err != nil {
		handleError(writer, r, http.StatusInternalServerError, err)
		return
	}

	s.HandleConnection(r, conn, nil)
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
		agentServer:    agentServer,
		logger:         logger,
		workers:        make(map[string]*agent.Worker),
		namespaces:     make(map[string]*namespaceInfo),
		serverInfo:     serverInfo,
		keyProvider:    keyProvider,
		roomTopic:      roomTopic,
		publisherTopic: publisherTopic,
	}
}

func (h *AgentHandler) HandleConnection(r *http.Request, conn *websocket.Conn, onIdle func()) {
	var protocol agent.WorkerProtocolVersion
	if pv, err := strconv.Atoi(r.FormValue("protocol")); err == nil {
		protocol = agent.WorkerProtocolVersion(pv)
	}

	sigConn := NewWSSignalConnection(conn)

	apiKey := GetAPIKey(r.Context())
	apiSecret := h.keyProvider.GetSecret(apiKey)

	worker := agent.NewWorker(protocol, apiKey, apiSecret, h.serverInfo, conn, sigConn, h.logger)
	worker.OnWorkerRegistered(h.handleWorkerRegister)

	h.mu.Lock()
	h.workers[worker.ID()] = worker
	h.mu.Unlock()

	defer func() {
		worker.Close()

		h.mu.Lock()
		delete(h.workers, worker.ID())
		numWorkers := len(h.workers)
		h.mu.Unlock()

		if worker.Registered() {
			h.handleWorkerDeregister(worker)
		}

		if numWorkers == 0 && onIdle != nil {
			onIdle()
		}
	}()

	for {
		req, _, err := sigConn.ReadWorkerMessage()
		if err != nil {
			// normal/expected closure
			if err == io.EOF ||
				strings.HasSuffix(err.Error(), "use of closed network connection") ||
				strings.HasSuffix(err.Error(), "connection reset by peer") ||
				websocket.IsCloseError(
					err,
					websocket.CloseAbnormalClosure,
					websocket.CloseGoingAway,
					websocket.CloseNormalClosure,
					websocket.CloseNoStatusReceived,
				) {
				worker.Logger.Infow("worker closed WS connection", "wsError", err)
			} else {
				worker.Logger.Errorw("error reading from websocket", err)
			}
			return
		}

		worker.HandleMessage(req)
	}
}

func (h *AgentHandler) handleWorkerRegister(w *agent.Worker) {
	h.mu.Lock()

	info, ok := h.namespaces[w.Namespace()]
	numPublishers := int32(0)
	numRooms := int32(0)
	if ok {
		numPublishers = info.numPublishers
		numRooms = info.numRooms
	}

	shouldNotify := false
	var err error
	if w.JobType() == livekit.JobType_JT_PUBLISHER {
		numPublishers++
		if numPublishers == 1 {
			shouldNotify = true
			err = h.agentServer.RegisterJobRequestTopic(w.Namespace(), h.publisherTopic)
		}

	} else if w.JobType() == livekit.JobType_JT_ROOM {
		numRooms++
		if numRooms == 1 {
			shouldNotify = true
			err = h.agentServer.RegisterJobRequestTopic(w.Namespace(), h.roomTopic)
		}
	}

	if err != nil {
		w.Logger.Errorw("failed to register job request topic", err)
		h.mu.Unlock()
		w.Close() // Close the worker
		return
	}

	h.namespaces[w.Namespace()] = &namespaceInfo{
		numPublishers: numPublishers,
		numRooms:      numRooms,
	}

	h.roomEnabled = h.roomAvailableLocked()
	h.publisherEnabled = h.publisherAvailableLocked()
	h.mu.Unlock()

	if shouldNotify {
		h.logger.Infow("initial worker registered", "namespace", w.Namespace(), "jobType", w.JobType())
		err = h.agentServer.PublishWorkerRegistered(context.Background(), agent.DefaultHandlerNamespace, &emptypb.Empty{})
		if err != nil {
			w.Logger.Errorw("failed to publish worker registered", err)
		}
	}
}

func (h *AgentHandler) handleWorkerDeregister(worker *agent.Worker) {
	h.mu.Lock()
	defer h.mu.Unlock()

	info, ok := h.namespaces[worker.Namespace()]
	if !ok {
		return
	}

	if worker.JobType() == livekit.JobType_JT_PUBLISHER {
		info.numPublishers--
		if info.numPublishers == 0 {
			h.agentServer.DeregisterJobRequestTopic(worker.Namespace(), h.publisherTopic)
		}
	} else if worker.JobType() == livekit.JobType_JT_ROOM {
		info.numRooms--
		if info.numRooms == 0 {
			h.agentServer.DeregisterJobRequestTopic(worker.Namespace(), h.roomTopic)
		}
	}

	if info.numPublishers == 0 && info.numRooms == 0 {
		h.logger.Debugw("last worker deregistered")
		delete(h.namespaces, worker.Namespace())
	}

	h.roomEnabled = h.roomAvailableLocked()
	h.publisherEnabled = h.publisherAvailableLocked()
}

func (h *AgentHandler) roomAvailableLocked() bool {
	for _, w := range h.workers {
		if w.JobType() == livekit.JobType_JT_ROOM {
			return true
		}
	}
	return false

}

func (h *AgentHandler) publisherAvailableLocked() bool {
	for _, w := range h.workers {
		if w.JobType() == livekit.JobType_JT_PUBLISHER {
			return true
		}
	}

	return false
}

func (h *AgentHandler) JobRequest(ctx context.Context, job *livekit.Job) (*emptypb.Empty, error) {
	attempted := make(map[string]bool)
	for {
		h.mu.Lock()
		var selected *agent.Worker
		var maxLoad float32
		for _, w := range h.workers {
			if w.Namespace() != job.Namespace || w.JobType() != job.Type {
				continue
			}

			_, ok := attempted[w.ID()]
			if ok {
				continue
			}

			if w.Status() == livekit.WorkerStatus_WS_AVAILABLE {
				load := w.Load()
				if len(w.RunningJobs()) > 0 && load > maxLoad {
					maxLoad = load
					selected = w
				} else if selected == nil {
					selected = w
				}
			}

		}
		h.mu.Unlock()

		if selected == nil {
			return nil, psrpc.NewErrorf(psrpc.DeadlineExceeded, "no workers available")
		}

		attempted[selected.ID()] = true

		values := []interface{}{
			"jobID", job.Id,
			"namespace", job.Namespace,
			"workerID", selected.ID(),
		}
		if job.Room != nil {
			values = append(values, "room", job.Room.Name, "roomID", job.Room.Sid)
		}
		if job.Participant != nil {
			values = append(values, "participant", job.Participant.Identity)
		}
		logger.Debugw("assigning job", values...)
		err := selected.AssignJob(ctx, job)
		if err != nil {
			if errors.Is(err, agent.ErrWorkerNotAvailable) {
				continue // Try another worker
			}
			return nil, err
		}

		return &emptypb.Empty{}, nil
	}

}

func (h *AgentHandler) JobRequestAffinity(ctx context.Context, job *livekit.Job) float32 {
	h.mu.Lock()
	defer h.mu.Unlock()

	var affinity float32
	var maxLoad float32
	for _, w := range h.workers {
		if w.Namespace() != job.Namespace || w.JobType() != job.Type {
			continue
		}

		if w.Status() == livekit.WorkerStatus_WS_AVAILABLE {
			load := w.Load()
			if len(w.RunningJobs()) > 0 && load > maxLoad {
				maxLoad = load
				affinity = 0.5 + load/2
			} else {
				affinity = 0.5
			}
		}

	}

	return affinity
}

func (h *AgentHandler) CheckEnabled(ctx context.Context, req *rpc.CheckEnabledRequest) (*rpc.CheckEnabledResponse, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	namespaces := make([]string, 0, len(h.namespaces))
	for ns := range h.namespaces {
		namespaces = append(namespaces, ns)
	}

	return &rpc.CheckEnabledResponse{
		Namespaces:       namespaces,
		RoomEnabled:      h.roomEnabled,
		PublisherEnabled: h.publisherEnabled,
	}, nil
}

func (h *AgentHandler) NumConnections() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.workers)
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
