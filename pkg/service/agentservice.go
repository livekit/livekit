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
	"github.com/livekit/livekit-server/pkg/agent"
	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/rtc"
	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/utils"
	"github.com/livekit/livekit-server/version"
	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/psrpc"
	"google.golang.org/protobuf/types/known/emptypb"
)

type AgentService struct {
	upgrader websocket.Upgrader

	*AgentHandler
}

type AgentHandler struct {
	agentServer rpc.AgentInternalServer
	mu          sync.Mutex

	serverInfo  *livekit.ServerInfo
	workers     map[string]*agent.Worker
	keyProvider auth.KeyProvider

	namespaces       map[string]*namespaceInfo
	publisherEnabled bool
	roomEnabled      bool
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
	s.AgentHandler = NewAgentHandler(agentServer, keyProvider, serverInfo)
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

	s.HandleConnection(r, conn)
}

func (s *AgentService) HandleConnection(r *http.Request, conn *websocket.Conn) {
	var protocol agent.WorkerProtocolVersion
	if pv, err := strconv.Atoi(r.Form.Get("protocol")); err == nil {
		protocol = agent.WorkerProtocolVersion(pv)
	}

	sigConn := NewWSSignalConnection(conn)

	logger := utils.GetLogger(r.Context())

	apiKey := GetAPIKey(r.Context())
	apiSecret := s.keyProvider.GetSecret(apiKey)

	worker := agent.NewWorker(protocol, apiKey, apiSecret, s.serverInfo, logger)
	worker.OnWorkerRegistered(s.registerWorkerTopic)

	s.mu.Lock()
	s.workers[worker.ID()] = worker
	s.mu.Unlock()

	defer func() {
		worker.Close()

		s.mu.Lock()
		delete(s.workers, worker.ID())
		s.mu.Unlock()

		if worker.Registered() {
			s.unregisterWorkerTopic(worker)
		}
	}()

	go func() {
		defer func() {
			_ = conn.Close()
		}()

		for msg := range worker.ReadChan() {
			if _, err := sigConn.WriteServerMessage(msg); err != nil {
				worker.Logger.Errorw("error writing to websocket", err)
				return
			}
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
				worker.Logger.Infow("Agent worker closed WS connection", "wsError", err)
			} else {
				worker.Logger.Errorw("error reading from websocket", err)
			}
			return
		}

		worker.HandleMessage(req)
	}
}

func NewAgentHandler(agentServer rpc.AgentInternalServer, keyProvider auth.KeyProvider, serverInfo *livekit.ServerInfo) *AgentHandler {
	return &AgentHandler{
		agentServer: agentServer,
		workers:     make(map[string]*agent.Worker),
		namespaces:  make(map[string]*namespaceInfo),
		serverInfo:  serverInfo,
		keyProvider: keyProvider,
	}
}

func (h *AgentHandler) registerWorkerTopic(w *agent.Worker) {
	h.mu.Lock()
	defer h.mu.Unlock()

	info, ok := h.namespaces[w.Namespace()]
	numPublishers := int32(0)
	numRooms := int32(0)
	if ok {
		numPublishers = info.numPublishers
		numRooms = info.numRooms
	}

	var err error
	if w.JobType() == livekit.JobType_JT_PUBLISHER {
		numPublishers++
		if numPublishers == 1 {
			err = h.agentServer.RegisterJobRequestTopic(w.Namespace(), agent.PublisherAgentTopic)
		}

	} else if w.JobType() == livekit.JobType_JT_ROOM {
		numRooms++
		if numRooms == 1 {
			err = h.agentServer.RegisterJobRequestTopic(w.Namespace(), agent.RoomAgentTopic)
		}
	}

	if err != nil {
		w.Logger.Errorw("failed to register job request topic", err)
		w.Close() // Close the worker
		return
	}

	h.namespaces[w.Namespace()] = &namespaceInfo{
		numPublishers: numPublishers,
		numRooms:      numRooms,
	}

	h.roomEnabled = h.roomAvailableLocked()
	h.publisherEnabled = h.publisherAvailableLocked()
}

func (h *AgentHandler) unregisterWorkerTopic(worker *agent.Worker) {
	h.mu.Lock()
	defer h.mu.Unlock()

	info, ok := h.namespaces[worker.Namespace()]
	if !ok {
		return
	}

	if worker.JobType() == livekit.JobType_JT_PUBLISHER {
		info.numPublishers--
		if info.numPublishers == 0 {
			h.agentServer.DeregisterJobRequestTopic(worker.Namespace(), agent.PublisherAgentTopic)
		}
	} else if worker.JobType() == livekit.JobType_JT_ROOM {
		info.numRooms--
		if info.numRooms == 0 {
			h.agentServer.DeregisterJobRequestTopic(worker.Namespace(), agent.RoomAgentTopic)
		}
	}

	if info.numPublishers == 0 && info.numRooms == 0 {
		delete(h.namespaces, worker.Namespace())
	}

	h.roomEnabled = h.roomAvailableLocked()
	h.publisherEnabled = h.publisherAvailableLocked()
}

func (s *AgentHandler) roomAvailableLocked() bool {
	for _, w := range s.workers {
		if w.JobType() == livekit.JobType_JT_ROOM {
			return true
		}
	}
	return false

}

func (s *AgentHandler) publisherAvailableLocked() bool {
	for _, w := range s.workers {
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
		for _, w := range h.workers {
			if job.Type != w.JobType() {
				continue
			}

			if job.Namespace != w.Namespace() {
				continue
			}

			if _, ok := attempted[w.ID()]; ok {
				continue
			}

			if w.Status() == livekit.WorkerStatus_WS_AVAILABLE {
				if len(w.RunningJobs()) > 0 {
					selected = w
					break
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

		err := selected.AssignJob(ctx, job)
		if err != nil && !errors.Is(err, agent.ErrWorkerNotAvailable) {
			return nil, err
		}
	}
}

func (h *AgentHandler) JobRequestAffinity(ctx context.Context, job *livekit.Job) float32 {
	h.mu.Lock()
	defer h.mu.Unlock()

	var affinity float32
	for _, w := range h.workers {
		if w.Namespace() != job.Namespace {
			continue
		}

		if w.Status() == livekit.WorkerStatus_WS_AVAILABLE {
			if len(w.RunningJobs()) > 0 {
				return 1
			} else {
				affinity = 0.5
			}
		}

	}

	return affinity
}

func (h *AgentHandler) CheckEnabled(ctx context.Context, req *rpc.CheckEnabledRequest) (*rpc.CheckEnabledResponse, error) {
	namespaces := make([]string, len(h.namespaces))
	for ns := range h.namespaces {
		namespaces = append(namespaces, ns)
	}

	return &rpc.CheckEnabledResponse{
		Namespaces:       namespaces,
		RoomEnabled:      h.roomEnabled,
		PublisherEnabled: h.publisherEnabled,
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
