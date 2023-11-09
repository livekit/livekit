// Copyright 2023 LiveKit, Inc.
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
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/livekit/livekit-server/pkg/rtc"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/psrpc"
)

const AgentServiceVersion = "0.1.0"

type AgentService struct {
	upgrader websocket.Upgrader

	*AgentHandler
}

type AgentHandler struct {
	agentServer    rpc.AgentInternalServer
	roomTopic      string
	publisherTopic string

	mu                  sync.Mutex
	availability        map[string]chan *availability
	unregistered        map[*websocket.Conn]*worker
	roomRegistered      bool
	roomWorkers         map[string]*worker
	publisherRegistered bool
	publisherWorkers    map[string]*worker
}

type worker struct {
	mu      sync.Mutex
	conn    *websocket.Conn
	sigConn *WSSignalConnection

	id         string
	jobType    livekit.JobType
	status     livekit.WorkerStatus
	activeJobs int
}

type availability struct {
	workerID  string
	available bool
}

func NewAgentService(bus psrpc.MessageBus) (*AgentService, error) {
	s := &AgentService{
		upgrader: websocket.Upgrader{},
	}

	// allow connections from any origin, since script may be hosted anywhere
	// security is enforced by access tokens
	s.upgrader.CheckOrigin = func(r *http.Request) bool {
		return true
	}

	agentServer, err := rpc.NewAgentInternalServer(s, bus)
	if err != nil {
		return nil, err
	}
	s.AgentHandler = NewAgentHandler(agentServer, rtc.RoomAgentTopic, rtc.PublisherAgentTopic)

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
		handleError(writer, http.StatusUnauthorized, rtc.ErrPermissionDenied)
		return
	}

	// upgrade
	conn, err := s.upgrader.Upgrade(writer, r, nil)
	if err != nil {
		handleError(writer, http.StatusInternalServerError, err)
		return
	}

	s.HandleConnection(conn)
}

func NewAgentHandler(agentServer rpc.AgentInternalServer, roomTopic, publisherTopic string) *AgentHandler {
	return &AgentHandler{
		agentServer:      agentServer,
		roomTopic:        roomTopic,
		publisherTopic:   publisherTopic,
		availability:     make(map[string]chan *availability),
		unregistered:     make(map[*websocket.Conn]*worker),
		roomWorkers:      make(map[string]*worker),
		publisherWorkers: make(map[string]*worker),
	}
}

func (s *AgentHandler) HandleConnection(conn *websocket.Conn) {
	sigConn := NewWSSignalConnection(conn)
	w := &worker{
		conn:    conn,
		sigConn: sigConn,
	}

	s.mu.Lock()
	s.unregistered[conn] = w
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		if w.id == "" {
			delete(s.unregistered, conn)
		} else {
			switch w.jobType {
			case livekit.JobType_JT_ROOM:
				delete(s.roomWorkers, w.id)
				if s.roomRegistered && !s.roomAvailableLocked() {
					s.roomRegistered = false
					s.agentServer.DeregisterJobRequestTopic(s.roomTopic)
				}
			case livekit.JobType_JT_PUBLISHER:
				delete(s.publisherWorkers, w.id)
				if s.publisherRegistered && !s.publisherAvailableLocked() {
					s.publisherRegistered = false
					s.agentServer.DeregisterJobRequestTopic(s.publisherTopic)
				}
			}
		}
		s.mu.Unlock()
	}()

	// handle incoming requests from websocket
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
				logger.Infow("exit ws read loop for closed connection", "wsError", err)
			} else {
				logger.Errorw("error reading from websocket", err)
			}
			return
		}

		switch m := req.Message.(type) {
		case *livekit.WorkerMessage_Register:
			go s.handleRegister(w, m.Register)
		case *livekit.WorkerMessage_Availability:
			go s.handleAvailability(w, m.Availability)
		case *livekit.WorkerMessage_JobUpdate:
			go s.handleJobUpdate(w, m.JobUpdate)
		case *livekit.WorkerMessage_Status:
			go s.handleStatus(w, m.Status)
		}
	}
}

func (s *AgentHandler) handleRegister(worker *worker, msg *livekit.RegisterWorkerRequest) {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch msg.Type {
	case livekit.JobType_JT_ROOM:
		worker.id = msg.WorkerId
		delete(s.unregistered, worker.conn)
		s.roomWorkers[worker.id] = worker

		if !s.roomRegistered {
			err := s.agentServer.RegisterJobRequestTopic(s.roomTopic)
			if err != nil {
				logger.Errorw("failed to register room agents", err)
			} else {
				s.roomRegistered = true
			}
		}

	case livekit.JobType_JT_PUBLISHER:
		worker.id = msg.WorkerId
		delete(s.unregistered, worker.conn)
		s.publisherWorkers[worker.id] = worker

		if !s.publisherRegistered {
			err := s.agentServer.RegisterJobRequestTopic(s.publisherTopic)
			if err != nil {
				logger.Errorw("failed to register publisher agents", err)
			} else {
				s.publisherRegistered = true
			}
		}
	}

	_, err := worker.sigConn.WriteServerMessage(&livekit.ServerMessage{
		Message: &livekit.ServerMessage_Register{
			Register: &livekit.RegisterWorkerResponse{
				WorkerId:      worker.id,
				ServerVersion: AgentServiceVersion,
			},
		},
	})
	if err != nil {
		logger.Errorw("failed to write server message", err)
	}
}

func (s *AgentHandler) handleAvailability(w *worker, msg *livekit.AvailabilityResponse) {
	s.mu.Lock()
	availabilityChan, ok := s.availability[msg.JobId]
	s.mu.Unlock()

	if ok {
		availabilityChan <- &availability{
			workerID:  w.id,
			available: msg.Available,
		}
	}
}

func (s *AgentHandler) handleJobUpdate(w *worker, msg *livekit.JobStatusUpdate) {
	switch msg.Status {
	case livekit.JobStatus_JS_SUCCESS:
		logger.Debugw("job complete", "jobID", msg.JobId)
	case livekit.JobStatus_JS_FAILED:
		logger.Warnw("job failed", errors.New(msg.Error), "jobID", msg.JobId)
	}

	w.mu.Lock()
	w.activeJobs--
	w.mu.Unlock()
}

func (s *AgentHandler) handleStatus(w *worker, msg *livekit.UpdateWorkerStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()

	w.mu.Lock()
	w.status = msg.Status
	w.mu.Unlock()

	switch w.jobType {
	case livekit.JobType_JT_ROOM:
		if s.roomRegistered && !s.roomAvailableLocked() {
			s.roomRegistered = false
			s.agentServer.DeregisterJobRequestTopic(s.roomTopic)
		} else if !s.roomRegistered && s.roomAvailableLocked() {
			if err := s.agentServer.RegisterJobRequestTopic(s.roomTopic); err != nil {
				logger.Errorw("failed to register room agents", err)
			} else {
				s.roomRegistered = true
			}
		}
	case livekit.JobType_JT_PUBLISHER:
		if s.publisherRegistered && !s.publisherAvailableLocked() {
			s.publisherRegistered = false
			s.agentServer.DeregisterJobRequestTopic(s.publisherTopic)
		} else if !s.publisherRegistered && s.publisherAvailableLocked() {
			if err := s.agentServer.RegisterJobRequestTopic(s.publisherTopic); err != nil {
				logger.Errorw("failed to register publisher agents", err)
			} else {
				s.publisherRegistered = true
			}
		}
	}
}

func (s *AgentHandler) CheckEnabled(_ context.Context, _ *rpc.CheckEnabledRequest) (*rpc.CheckEnabledResponse, error) {
	s.mu.Lock()
	res := &rpc.CheckEnabledResponse{
		RoomEnabled:      len(s.roomWorkers) > 0,
		PublisherEnabled: len(s.publisherWorkers) > 0,
	}
	s.mu.Unlock()
	return res, nil
}

func (s *AgentHandler) JobRequest(ctx context.Context, job *livekit.Job) (*emptypb.Empty, error) {
	s.mu.Lock()
	ac := make(chan *availability, 100)
	s.availability[job.Id] = ac
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		delete(s.availability, job.Id)
		s.mu.Unlock()
	}()

	var pool map[string]*worker
	switch job.Type {
	case livekit.JobType_JT_ROOM:
		pool = s.roomWorkers
	case livekit.JobType_JT_PUBLISHER:
		pool = s.publisherWorkers
	}

	attempted := make(map[string]bool)
	for {
		select {
		case <-ctx.Done():
			return nil, psrpc.NewErrorf(psrpc.DeadlineExceeded, "request timed out")
		default:
			s.mu.Lock()
			var selected *worker
			for _, w := range pool {
				if attempted[w.id] {
					continue
				}
				if w.status == livekit.WorkerStatus_WS_AVAILABLE {
					if w.activeJobs > 0 {
						selected = w
						break
					} else if selected == nil {
						selected = w
					}
				}
			}
			s.mu.Unlock()

			if selected == nil {
				return nil, psrpc.NewErrorf(psrpc.Unavailable, "no workers available")
			}

			attempted[selected.id] = true
			_, err := selected.sigConn.WriteServerMessage(&livekit.ServerMessage{Message: &livekit.ServerMessage_Availability{
				Availability: &livekit.AvailabilityRequest{Job: job},
			}})
			if err != nil {
				logger.Errorw("failed to send availability request", err, "workerID", selected.id)
			}

			select {
			case <-ctx.Done():
				return nil, psrpc.NewErrorf(psrpc.DeadlineExceeded, "request timed out")
			case res := <-ac:
				if res.available {
					_, err = selected.sigConn.WriteServerMessage(&livekit.ServerMessage{Message: &livekit.ServerMessage_Assignment{
						Assignment: &livekit.JobAssignment{Job: job},
					}})
					if err != nil {
						logger.Errorw("failed to assign job", err, "workerID", selected.id)
					} else {
						selected.mu.Lock()
						selected.activeJobs++
						selected.mu.Unlock()
						return &emptypb.Empty{}, nil
					}
				}
			}
		}
	}
}

func (s *AgentHandler) JobRequestAffinity(ctx context.Context, job *livekit.Job) float32 {
	s.mu.Lock()
	defer s.mu.Unlock()

	var pool map[string]*worker
	switch job.Type {
	case livekit.JobType_JT_ROOM:
		pool = s.roomWorkers
	case livekit.JobType_JT_PUBLISHER:
		pool = s.publisherWorkers
	}

	var affinity float32
	for _, w := range pool {
		if w.status == livekit.WorkerStatus_WS_AVAILABLE {
			if w.activeJobs > 0 {
				return 1
			} else {
				affinity = 0.5
			}
		}
	}

	return affinity
}

func (s *AgentHandler) NumConnections() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return len(s.unregistered) + len(s.roomWorkers) + len(s.publisherWorkers)
}

func (s *AgentHandler) DrainConnections(interval time.Duration) {
	// jitter drain start
	time.Sleep(time.Duration(rand.Int63n(int64(interval))))

	t := time.NewTicker(interval)
	defer t.Stop()

	s.mu.Lock()
	defer s.mu.Unlock()

	for conn := range s.unregistered {
		_ = conn.Close()
		<-t.C
	}
	for _, w := range s.roomWorkers {
		_ = w.conn.Close()
		<-t.C
	}
	for _, w := range s.publisherWorkers {
		_ = w.conn.Close()
		<-t.C
	}
}

func (s *AgentHandler) roomAvailableLocked() bool {
	for _, w := range s.roomWorkers {
		if w.status == livekit.WorkerStatus_WS_AVAILABLE {
			return true
		}
	}
	return false
}

func (s *AgentHandler) publisherAvailableLocked() bool {
	for _, w := range s.publisherWorkers {
		if w.status == livekit.WorkerStatus_WS_AVAILABLE {
			return true
		}
	}
	return false
}
