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
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	httppprof "net/http/pprof"
	"runtime"
	"runtime/pprof"
	"strconv"
	"time"

	"github.com/pion/turn/v5"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/quic-go/webtransport-go"
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
	config             *config.Config
	ioService          *IOInfoService
	rtcService         *RTCService
	whipService        *WHIPService
	httpServer         *http.Server
	promServer         *http.Server
	debugServer        *http.Server
	webtransportServer *webtransport.Server
	router             routing.Router
	roomManager        *RoomManager
	signalServer       *SignalServer
	turnServer         *turn.Server
	currentNode        routing.LocalNode
	running            atomic.Bool
	doneChan           chan struct{}
	closedChan         chan struct{}
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
	agentWSService *AgentWSService,
	agentWTService *AgentWTService,
	agentEndpointService *AgentEndpointService,
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
		router:       router,
		roomManager:  roomManager,
		signalServer: signalServer,
		// turn server starts automatically
		turnServer:  turnServer,
		currentNode: currentNode,
		closedChan:  make(chan struct{}),
	}

	serverOptions := []any{
		twirp.WithServerHooks(twirp.ChainHooks(
			TwirpLogger(),
			TwirpEgressID(),
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
	mux.Handle("/agent", agentWSService)
	mux.HandleFunc("/", s.defaultHandler)

	// NewHTTPHandler branches on a nil handler, which a typed-nil would defeat
	var agentFront http.Handler
	if !conf.Agents.Endpoints.Disabled {
		agentFront = agentEndpointService
	}

	s.httpServer = &http.Server{
		Handler: NewHTTPHandler(conf, keyProvider, mux, agentFront),
	}

	if conf.PrometheusPort > 0 {
		logger.Warnw("prometheus_port is deprecated, please switch to prometheus.port instead", nil)
		conf.Prometheus.Port = conf.PrometheusPort
	}

	if conf.Prometheus.Port > 0 {
		promHandler := promhttp.Handler()
		if conf.Prometheus.Username != "" && conf.Prometheus.Password != "" {
			protectedHandler := negroni.New()
			protectedHandler.Use(negroni.HandlerFunc(GenBasicAuthMiddleware(conf.Prometheus.Username, conf.Prometheus.Password)))
			protectedHandler.UseHandler(promHandler)
			promHandler = protectedHandler
		} else if conf.Prometheus.Username != "" || conf.Prometheus.Password != "" {
			logger.Warnw("prometheus username or password is set but not both, set both or nothing for unauthenticated access", nil)
			err = errors.New("prometheus username or password is set but not both, set both or nothing for unauthenticated access")
			return
		}
		s.promServer = &http.Server{
			Handler: promHandler,
		}
	}

	if conf.DebugHandler.Port > 0 {
		debugMux := http.NewServeMux()
		debugMux.HandleFunc("/debug/pprof/", httppprof.Index)
		debugMux.HandleFunc("/debug/pprof/cmdline", httppprof.Cmdline)
		debugMux.HandleFunc("/debug/pprof/profile", httppprof.Profile)
		debugMux.HandleFunc("/debug/pprof/symbol", httppprof.Symbol)
		debugMux.HandleFunc("/debug/pprof/trace", httppprof.Trace)
		debugMux.HandleFunc("/debug/goroutine", s.debugGoroutines)
		debugMux.HandleFunc("/debug/rooms", s.debugInfo)
		s.debugServer = &http.Server{
			Handler: http.Handler(debugMux),
		}
	}

	if conf.WebTransport.Port > 0 {
		var tlsConf *tls.Config
		tlsConf, err = WebTransportTLS(conf.WebTransport.TLSCertFile, conf.WebTransport.TLSKeyFile, conf.Development)
		if err != nil {
			return
		}
		wtMux := http.NewServeMux()
		wtMux.Handle("/agent", agentWTService)

		s.webtransportServer = NewWebTransportServer(tlsConf)
		s.webtransportServer.H3.Handler = NewWebTransportHandler(keyProvider, s.webtransportServer, wtMux)
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
	debugListeners := make([]net.Listener, 0)
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

		if s.debugServer != nil {
			ln, err = net.Listen("tcp", net.JoinHostPort(addr, strconv.Itoa(int(s.config.DebugHandler.Port))))
			if err != nil {
				return err
			}
			debugListeners = append(debugListeners, ln)
		}
	}

	stopWebTransport := func() {}
	if s.webtransportServer != nil {
		_, stop, err := ListenWebTransport(s.webtransportServer, s.config.BindAddresses, s.config.WebTransport.Port)
		if err != nil {
			return err
		}
		stopWebTransport = stop
	}

	values := []any{
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
	if s.config.DebugHandler.Port != 0 {
		values = append(values, "portDebugHandler", s.config.DebugHandler.Port)
	}
	if s.config.WebTransport.Port != 0 {
		values = append(values, "portWebTransport", s.config.WebTransport.Port)
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

	for _, debugLn := range debugListeners {
		go s.debugServer.Serve(debugLn)
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
	if s.debugServer != nil {
		_ = s.debugServer.Shutdown(ctx)
	}
	stopWebTransport()

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
	info := make([]map[string]any, 0, len(s.roomManager.rooms))
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
		_, _ = fmt.Fprintf(w, "Not Ready\nNode Updated At %s", updatedAt)
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

// NewHTTPHandler builds the node's public HTTP handler: the agent endpoint prefix
// on its own middleware chain, everything else on the API chain. A nil agentFront
// leaves the prefix unserved, and those paths fall through to apiHandler.
func NewHTTPHandler(conf *config.Config, keyProvider auth.KeyProvider, apiHandler, agentFront http.Handler) http.Handler {
	apiMiddlewares := []negroni.Handler{
		// always first
		negroni.NewRecovery(),
		// CORS is allowed, we rely on token authentication to prevent improper use
		cors.New(corsOptions([]string{"OPTIONS", "HEAD", "GET", "POST", "PATCH", "DELETE"})),
		// limit request body size so large messages cannot exhaust memory
		NewRequestBodyLimiter(conf.Limit.MaxAPIRequestBodySize),
	}
	// this chain must not swallow http.ErrAbortHandler or bound the request body
	agentMiddlewares := []negroni.Handler{
		negroni.HandlerFunc(AgentRecovery),
		// the methods a manifest may declare, less TRACE, which browsers forbid in CORS
		cors.New(corsOptions([]string{"OPTIONS", "HEAD", "GET", "POST", "PUT", "PATCH", "DELETE"})),
	}
	if keyProvider != nil {
		authMiddleware := NewAPIKeyAuthMiddleware(keyProvider)
		apiMiddlewares = append(apiMiddlewares, authMiddleware)
		agentMiddlewares = append(agentMiddlewares, authMiddleware)
	}

	api := configureMiddlewares(apiHandler, apiMiddlewares...)
	var dispatch http.Handler = api
	if agentFront != nil {
		agents := configureMiddlewares(agentFront, agentMiddlewares...)
		dispatch = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if IsAgentEndpointPath(r.URL.EscapedPath()) {
				agents.ServeHTTP(w, r)
				return
			}
			api.ServeHTTP(w, r)
		})
	}
	return WithPathNormalization(dispatch)
}

func corsOptions(methods []string) cors.Options {
	return cors.Options{
		AllowOriginFunc: func(origin string) bool {
			return true
		},
		AllowedMethods: methods,
		AllowedHeaders: []string{"*"},
		ExposedHeaders: []string{"*"},
		// allow preflight to be cached for a day
		MaxAge: 86400,
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
