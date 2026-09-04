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
	httppprof "net/http/pprof"
	"runtime"
	"runtime/pprof"
	"strconv"
	"time"

	"github.com/pion/turn/v5"
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

const (
	livenessPath  = "/healthz"
	readinessPath = "/readyz"

	// never call a node unready sooner than `GET /` always has
	minKeepaliveMaxDelay = 4 * time.Second

	// how often a draining node looks at what is left to drain
	drainPollInterval = 5 * time.Second
)

type LivekitServer struct {
	config       *config.Config
	ioService    *IOInfoService
	rtcService   *RTCService
	whipService  *WHIPService
	agentService *AgentService
	httpServer   *http.Server
	promServer   *http.Server
	debugServer  *http.Server
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
		// limit request body size so large messages cannot exhaust memory
		NewRequestBodyLimiter(conf.Limit.MaxAPIRequestBodySize),
	}
	if keyProvider != nil {
		middlewares = append(middlewares, NewAPIKeyAuthMiddleware(keyProvider))
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
	mux.Handle("/agent", agentService)
	s.setupHealthRoutes(mux)
	mux.HandleFunc("/", s.defaultHandler)

	s.httpServer = &http.Server{
		Handler: configureMiddlewares(mux, middlewares...),
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
	if !force {
		if s.roomManager.HasParticipants() {
			// says which deadlines are in play, so that whoever finds the node
			// sitting in the wait below knows what will end it and what to set
			logger.Infow("draining participants before shutdown",
				"nodeID", s.currentNode.NodeID(),
				"drainTimeout", s.config.Shutdown.DrainTimeout,
				"unreachableDrainTimeout", s.config.Shutdown.UnreachableDrainTimeout)
		}
		waitForDrain(
			s.config.Shutdown,
			drainPollInterval,
			s.roomManager.HasParticipants,
			s.currentNode.SecondsSinceKeepalive,
		)
	}

	if !s.running.Swap(false) {
		return
	}

	s.router.Stop()
	close(s.doneChan)

	// wait for fully closed
	<-s.closedChan
}

// waitForDrain blocks while participants remain on this node, and returns when
// they have left or when one of the configured deadlines runs out.
func waitForDrain(
	conf config.ShutdownConfig,
	poll time.Duration,
	hasParticipants func() bool,
	secondsSinceKeepalive func() float64,
) {
	if !hasParticipants() {
		return
	}

	ticker := time.NewTicker(poll)
	defer ticker.Stop()

	var deadline <-chan time.Time
	if conf.DrainTimeout > 0 {
		timer := time.NewTimer(conf.DrainTimeout)
		defer timer.Stop()
		deadline = timer.C
	}

	for {
		select {
		case <-ticker.C:
		case <-deadline:
			logger.Warnw("drain timed out, shutting down with participants still connected", nil,
				"drainTimeout", conf.DrainTimeout)
			return
		}

		if !hasParticipants() {
			return
		}

		if conf.UnreachableDrainTimeout > 0 {
			// the keepalive clock is reset by a ping that makes it back, so it
			// reads how long the node has been unreachable, not how long it has
			// been draining
			if delay := secondsSinceKeepalive(); delay > conf.UnreachableDrainTimeout.Seconds() {
				logger.Warnw("node has not heard its own keepalive, shutting down mid-drain", nil,
					"keepaliveDelay", delay,
					"unreachableDrainTimeout", conf.UnreachableDrainTimeout)
				return
			}
		}

		logger.Infow("waiting for participants to exit")
	}
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

func (s *LivekitServer) setupHealthRoutes(mux *http.ServeMux) {
	mux.HandleFunc(livenessPath, s.livenessCheck)
	mux.HandleFunc(readinessPath, s.readinessCheck)
}

// livenessCheck answers for this process and nothing else. It deliberately
// looks at nothing shared: a redis outage stalls every node at once, and a probe
// that failed on it would have kubernetes restart the whole fleet.
func (s *LivekitServer) livenessCheck(w http.ResponseWriter, _ *http.Request) {
	// the router samples stats on a local ticker, so their age is how long this
	// process has gone without making progress. a node told to sample less
	// often than the delay allows for is not stalled, it is configured that way
	maxDelay := max(2*s.config.NodeStats.StatsUpdateInterval, s.config.NodeStats.StatsMaxDelay)
	if delay := s.currentNode.SecondsSinceNodeStatsUpdate(); delay > maxDelay.Seconds() {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = fmt.Fprintf(w, "Not Alive\nNode Stats %.1fs Old", delay)
		return
	}

	writeOK(w)
}

// readinessCheck answers whether this node should be given work.
func (s *LivekitServer) readinessCheck(w http.ResponseWriter, _ *http.Request) {
	if delay, stale := s.keepaliveDelay(); stale {
		writeNotReady(w, http.StatusServiceUnavailable, fmt.Sprintf("Keepalive %.1fs Old", delay))
		return
	}

	if state := s.currentNode.State(); state != livekit.NodeState_SERVING {
		writeNotReady(w, http.StatusServiceUnavailable, fmt.Sprintf("Node Is %s", state))
		return
	}

	writeOK(w)
}

// healthCheck is what `GET /` has always answered, unchanged: the keepalive
// check under its own status code, and no state check, since deployments that
// predate the two probes above point their liveness at it.
func (s *LivekitServer) healthCheck(w http.ResponseWriter, _ *http.Request) {
	if delay, stale := s.keepaliveDelay(); stale {
		writeNotReady(w, http.StatusNotAcceptable, fmt.Sprintf("Keepalive %.1fs Old", delay))
		return
	}

	writeOK(w)
}

// keepaliveDelay reports how long since the node last heard its own keepalive,
// and whether that is too long. A node that stops hearing it cannot route
// signalling to itself either.
func (s *LivekitServer) keepaliveDelay() (float64, bool) {
	// a ping goes out on every stats update, so allow for two of them
	maxDelay := max(2*s.config.NodeStats.StatsUpdateInterval, minKeepaliveMaxDelay)
	delay := s.currentNode.SecondsSinceKeepalive()
	return delay, delay > maxDelay.Seconds()
}

func writeNotReady(w http.ResponseWriter, status int, reason string) {
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, "Not Ready\n%s", reason)
}

func writeOK(w http.ResponseWriter) {
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
