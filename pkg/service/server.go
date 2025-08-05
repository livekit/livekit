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
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	_ "net/http/pprof"
	"runtime"
	"runtime/pprof"
	"strconv"
	"time"

	"github.com/pion/turn/v4"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/cors"
	"github.com/twitchtv/twirp"
	"github.com/urfave/negroni/v3"
	"go.uber.org/atomic"
	"golang.org/x/sync/errgroup"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils/xtwirp"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/version"
)

type LivekitServer struct {
	config       *config.Config
	ioService    *IOInfoService
	rtcService   *RTCService
	whipService  *WHIPService
	agentService *AgentService
	httpServer   *http.Server
	promServer   *http.Server
	router       routing.Router
	roomManager  *RoomManager
	signalServer *SignalServer
	turnServer   *turn.Server
	currentNode  routing.LocalNode
	running      atomic.Bool
	doneChan     chan struct{}
	closedChan   chan struct{}
}

func NewLivekitServer(conf *config.Config,
	roomService livekit.RoomService,
	agentDispatchService *AgentDispatchService,
	egressService *EgressService,
	ingressService *IngressService,
	sipService *SIPService,
	ioService *IOInfoService,
	rtcService *RTCService,
	whipService *WHIPService,
	agentService *AgentService,
	keyProvider auth.KeyProvider,
	router routing.Router,
	roomManager *RoomManager,
	signalServer *SignalServer,
	turnServer *turn.Server,
	currentNode routing.LocalNode,
) (s *LivekitServer, err error) {
	s = &LivekitServer{
		config:       conf,
		ioService:    ioService,
		rtcService:   rtcService,
		whipService:  whipService,
		agentService: agentService,
		router:       router,
		roomManager:  roomManager,
		signalServer: signalServer,
		// turn server starts automatically
		turnServer:  turnServer,
		currentNode: currentNode,
		closedChan:  make(chan struct{}),
	}

	middlewares := []negroni.Handler{
		// always first
		negroni.NewRecovery(),
		// CORS is allowed, we rely on token authentication to prevent improper use
		cors.New(cors.Options{
			AllowOriginFunc: func(origin string) bool {
				return true
			},
			AllowedMethods: []string{"OPTIONS", "HEAD", "GET", "POST", "PATCH", "DELETE"},
			AllowedHeaders: []string{"*"},
			ExposedHeaders: []string{"*"},
			// allow preflight to be cached for a day
			MaxAge: 86400,
		}),
		negroni.HandlerFunc(RemoveDoubleSlashes),
	}
	if keyProvider != nil {
		middlewares = append(middlewares, NewAPIKeyAuthMiddleware(keyProvider))
	}

	serverOptions := []interface{}{
		twirp.WithServerHooks(twirp.ChainHooks(
			TwirpLogger(),
			TwirpRequestStatusReporter(),
		)),
	}
	for _, opt := range xtwirp.DefaultServerOptions() {
		serverOptions = append(serverOptions, opt)
	}
	roomServer := livekit.NewRoomServiceServer(roomService, serverOptions...)
	agentDispatchServer := livekit.NewAgentDispatchServiceServer(agentDispatchService, serverOptions...)
	egressServer := livekit.NewEgressServer(egressService, serverOptions...)
	ingressServer := livekit.NewIngressServer(ingressService, serverOptions...)
	sipServer := livekit.NewSIPServer(sipService, serverOptions...)

	mux := http.NewServeMux()
	if conf.Development {
		// pprof handlers are registered onto DefaultServeMux
		mux = http.DefaultServeMux
		mux.HandleFunc("/debug/goroutine", s.debugGoroutines)
		mux.HandleFunc("/debug/rooms", s.debugInfo)
	}

	xtwirp.RegisterServer(mux, roomServer)
	xtwirp.RegisterServer(mux, agentDispatchServer)
	xtwirp.RegisterServer(mux, egressServer)
	xtwirp.RegisterServer(mux, ingressServer)
	xtwirp.RegisterServer(mux, sipServer)
	rtcService.SetupRoutes(mux)
	whipService.SetupRoutes(mux)
	mux.Handle("/agent", agentService)
	mux.HandleFunc("/", s.defaultHandler)

	s.httpServer = &http.Server{
		Handler: configureMiddlewares(mux, middlewares...),
	}

	if conf.PrometheusPort > 0 {
		logger.Warnw("prometheus_port is deprecated, please switch prometheus.port instead", nil)
		conf.Prometheus.Port = conf.PrometheusPort
	}

	if conf.Prometheus.Port > 0 {
		promHandler := promhttp.Handler()
		if conf.Prometheus.Username != "" && conf.Prometheus.Password != "" {
			protectedHandler := negroni.New()
			protectedHandler.Use(negroni.HandlerFunc(GenBasicAuthMiddleware(conf.Prometheus.Username, conf.Prometheus.Password)))
			protectedHandler.UseHandler(promHandler)
			promHandler = protectedHandler
		}
		s.promServer = &http.Server{
			Handler: promHandler,
		}
	}

	if err = router.RemoveDeadNodes(); err != nil {
		return
	}

	return
}

func (s *LivekitServer) Node() *livekit.Node {
	return s.currentNode.Clone()
}

func (s *LivekitServer) HTTPPort() int {
	return int(s.config.Port)
}

func (s *LivekitServer) IsRunning() bool {
	return s.running.Load()
}

func (s *LivekitServer) Start() error {
	if s.running.Load() {
		return errors.New("already running")
	}
	s.doneChan = make(chan struct{})

	if err := s.router.RegisterNode(); err != nil {
		return err
	}
	defer func() {
		if err := s.router.UnregisterNode(); err != nil {
			logger.Errorw("could not unregister node", err)
		}
	}()

	if err := s.router.Start(); err != nil {
		return err
	}

	if err := s.ioService.Start(); err != nil {
		return err
	}

	addresses := s.config.BindAddresses
	if addresses == nil {
		addresses = []string{""}
	}

	// ensure we could listen
	listeners := make([]net.Listener, 0)
	promListeners := make([]net.Listener, 0)
	for _, addr := range addresses {
		ln, err := net.Listen("tcp", net.JoinHostPort(addr, strconv.Itoa(int(s.config.Port))))
		if err != nil {
			return err
		}
		listeners = append(listeners, ln)

		if s.promServer != nil {
			ln, err = net.Listen("tcp", net.JoinHostPort(addr, strconv.Itoa(int(s.config.Prometheus.Port))))
			if err != nil {
				return err
			}
			promListeners = append(promListeners, ln)
		}
	}

	values := []interface{}{
		"portHttp", s.config.Port,
		"nodeID", s.currentNode.NodeID(),
		"nodeIP", s.currentNode.NodeIP(),
		"version", version.Version,
	}
	if s.config.BindAddresses != nil {
		values = append(values, "bindAddresses", s.config.BindAddresses)
	}
	if s.config.RTC.TCPPort != 0 {
		values = append(values, "rtc.portTCP", s.config.RTC.TCPPort)
	}
	if !s.config.RTC.ForceTCP && s.config.RTC.UDPPort.Valid() {
		values = append(values, "rtc.portUDP", s.config.RTC.UDPPort)
	} else {
		values = append(values,
			"rtc.portICERange", []uint32{s.config.RTC.ICEPortRangeStart, s.config.RTC.ICEPortRangeEnd},
		)
	}
	if s.config.Prometheus.Port != 0 {
		values = append(values, "portPrometheus", s.config.Prometheus.Port)
	}
	if s.config.Region != "" {
		values = append(values, "region", s.config.Region)
	}
	logger.Infow("starting LiveKit server", values...)
	if runtime.GOOS == "windows" {
		logger.Infow("Windows detected, capacity management is unavailable")
	}

	for _, promLn := range promListeners {
		go s.promServer.Serve(promLn)
	}

	if err := s.signalServer.Start(); err != nil {
		return err
	}

	httpGroup := &errgroup.Group{}
	for _, ln := range listeners {
		l := ln
		httpGroup.Go(func() error {
			return s.httpServer.Serve(l)
		})
	}
	go func() {
		if err := httpGroup.Wait(); err != http.ErrServerClosed {
			logger.Errorw("could not start server", err)
			s.Stop(true)
		}
	}()

	go s.backgroundWorker()

	// give time for Serve goroutine to start
	time.Sleep(100 * time.Millisecond)

	s.running.Store(true)

	<-s.doneChan

	// wait for shutdown
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()
	_ = s.httpServer.Shutdown(ctx)

	if s.turnServer != nil {
		_ = s.turnServer.Close()
	}

	s.roomManager.Stop()
	s.signalServer.Stop()
	s.ioService.Stop()

	close(s.closedChan)
	return nil
}

func (s *LivekitServer) Stop(force bool) {
	// wait for all participants to exit
	s.router.Drain()
	partTicker := time.NewTicker(5 * time.Second)
	waitingForParticipants := !force && s.roomManager.HasParticipants()
	for waitingForParticipants {
		<-partTicker.C
		logger.Infow("waiting for participants to exit")
		waitingForParticipants = s.roomManager.HasParticipants()
	}
	partTicker.Stop()

	if !s.running.Swap(false) {
		return
	}

	s.router.Stop()
	close(s.doneChan)

	// wait for fully closed
	<-s.closedChan
}

func (s *LivekitServer) RoomManager() *RoomManager {
	return s.roomManager
}

func (s *LivekitServer) debugGoroutines(w http.ResponseWriter, _ *http.Request) {
	_ = pprof.Lookup("goroutine").WriteTo(w, 2)
}

func (s *LivekitServer) debugInfo(w http.ResponseWriter, _ *http.Request) {
	s.roomManager.lock.RLock()
	info := make([]map[string]interface{}, 0, len(s.roomManager.rooms))
	for _, room := range s.roomManager.rooms {
		info = append(info, room.DebugInfo())
	}
	s.roomManager.lock.RUnlock()

	b, err := json.Marshal(info)
	if err != nil {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(err.Error()))
	} else {
		_, _ = w.Write(b)
	}
}

func (s *LivekitServer) defaultHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/" {
		s.healthCheck(w, r)
	} else {
		http.NotFound(w, r)
	}
}

func (s *LivekitServer) healthCheck(w http.ResponseWriter, _ *http.Request) {
	var updatedAt time.Time
	if s.Node().Stats != nil {
		updatedAt = time.Unix(s.Node().Stats.UpdatedAt, 0)
	}
	if time.Since(updatedAt) > 4*time.Second {
		w.WriteHeader(http.StatusNotAcceptable)
		_, _ = w.Write([]byte(fmt.Sprintf("Not Ready\nNode Updated At %s", updatedAt)))
		return
	}

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("OK"))
}

// worker to perform periodic tasks per node
func (s *LivekitServer) backgroundWorker() {
	roomTicker := time.NewTicker(1 * time.Second)
	defer roomTicker.Stop()
	for {
		select {
		case <-s.doneChan:
			return
		case <-roomTicker.C:
			s.roomManager.CloseIdleRooms()
		}
	}
}

func configureMiddlewares(handler http.Handler, middlewares ...negroni.Handler) *negroni.Negroni {
	n := negroni.New()
	for _, m := range middlewares {
		n.Use(m)
	}
	n.UseHandler(handler)
	return n
}
