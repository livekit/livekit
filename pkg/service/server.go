package service

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	_ "net/http/pprof"
	"runtime/pprof"
	"time"

	p2p_database "github.com/dTelecom/p2p-realtime-database"
	"github.com/pion/turn/v2"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/cors"
	"github.com/twitchtv/twirp"
	"github.com/urfave/negroni/v3"
	"go.uber.org/atomic"
	"golang.org/x/sync/errgroup"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"golang.org/x/crypto/acme/autocert"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/version"
)

type LivekitServer struct {
	config         *config.Config
	rtcService     *RTCService
	httpServer     *http.Server
	httpsServer    *http.Server
	promServer     *http.Server
	router         routing.Router
	roomManager    *RoomManager
	signalServer   *SignalServer
	turnServer     *turn.Server
	currentNode    routing.LocalNode
	clientProvider *ClientProvider
	nodeProvider   *NodeProvider
	running        atomic.Bool
	doneChan       chan struct{}
	closedChan     chan struct{}
}

func NewLivekitServer(conf *config.Config,
	roomService livekit.RoomService,
	egressService *EgressService,
	ingressService *IngressService,
	rtcService *RTCService,
	keyProvider auth.KeyProviderPublicKey,
	router routing.Router,
	roomManager *RoomManager,
	signalServer *SignalServer,
	turnServer *turn.Server,
	currentNode routing.LocalNode,
	clientProvider *ClientProvider,
	nodeProvider *NodeProvider,
	db *p2p_database.DB,
	relevantNodesHandler *RelevantNodesHandler,
	mainDebugHandler *MainDebugHandler,
) (s *LivekitServer, err error) {
	s = &LivekitServer{
		config:       conf,
		rtcService:   rtcService,
		router:       router,
		roomManager:  roomManager,
		signalServer: signalServer,
		// turn server starts automatically
		turnServer:     turnServer,
		currentNode:    currentNode,
		clientProvider: clientProvider,
		nodeProvider:   nodeProvider,
		closedChan:     make(chan struct{}),
	}

	middlewares := []negroni.Handler{
		// always first
		negroni.NewRecovery(),
		// CORS is allowed, we rely on token authentication to prevent improper use
		cors.New(cors.Options{
			AllowOriginFunc: func(origin string) bool {
				return true
			},
			AllowedHeaders: []string{"*"},
			// allow preflight to be cached for a day
			MaxAge: 86400,
		}),
	}
	if keyProvider != nil {
		middlewares = append(middlewares, NewAPIKeyAuthMiddleware(clientProvider))
	}

	twirpLoggingHook := TwirpLogger(logger.GetLogger())
	twirpRequestStatusHook := TwirpRequestStatusReporter()
	roomServer := livekit.NewRoomServiceServer(roomService, twirpLoggingHook)
	egressServer := livekit.NewEgressServer(egressService, twirp.WithServerHooks(
		twirp.ChainHooks(
			twirpLoggingHook,
			twirpRequestStatusHook,
		),
	))
	ingressServer := livekit.NewIngressServer(ingressService, twirpLoggingHook)

	mux := http.NewServeMux()
	if conf.Development {
		// pprof handlers are registered onto DefaultServeMux
		mux = http.DefaultServeMux
		mux.HandleFunc("/debug/goroutine", s.debugGoroutines)
		mux.HandleFunc("/debug/rooms", s.debugInfo)
	}
	mux.Handle(roomServer.PathPrefix(), roomServer)
	mux.Handle(egressServer.PathPrefix(), egressServer)
	mux.Handle(ingressServer.PathPrefix(), ingressServer)
	mux.Handle("/rtc", rtcService)
	mux.HandleFunc("/rtc/validate", rtcService.Validate)
	mux.HandleFunc("/relevant", relevantNodesHandler.HTTPHandler)
	mux.HandleFunc("/node-debug", mainDebugHandler.nodeHTTPHandler)
	mux.HandleFunc("/peer-debug", mainDebugHandler.peerHTTPHandler)
	mux.HandleFunc("/client-debug", mainDebugHandler.clientHTTPHandler)
	mux.HandleFunc("/", s.defaultHandler)

	if conf.Domain != "" {
		certManager := autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			HostPolicy: autocert.HostWhitelist(conf.Domain),
		}

		dir := cacheDir()
		if dir != "" {
			certManager.Cache = autocert.DirCache(dir)
		}
		s.httpsServer = &http.Server{
			TLSConfig: &tls.Config{
				GetCertificate: certManager.GetCertificate,
			},
			Handler: configureMiddlewares(mux, middlewares...),
		}
		s.httpServer = &http.Server{
			Handler: certManager.HTTPHandler(nil),
		}
	} else {
		s.httpServer = &http.Server{
			Handler: configureMiddlewares(mux, middlewares...),
		}
	}

	if conf.PrometheusPort > 0 {
		s.promServer = &http.Server{
			Handler: promhttp.Handler(),
		}
	}

	var bindAddress string
	if len(conf.BindAddresses) == 0 {
		conf.LoggingP2P.Error("bind address expect value")
		bindAddress = "127.0.0.1"
	} else {
		bindAddress = conf.BindAddresses[0]
	}

	// clean up old rooms on startup
	if err = roomManager.CleanupRooms(); err != nil {
		return
	}
	if err = router.RemoveDeadNodes(); err != nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()

	err = nodeProvider.Save(ctx, Node{
		Id:     db.GetHost().ID().String(),
		Domain: conf.Domain,
		IP:     bindAddress,
	})
	if err != nil {
		conf.LoggingP2P.Errorf("node provider save error: %s", err)
	}

	return
}

func (s *LivekitServer) Node() *livekit.Node {
	return s.currentNode
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

	addresses := s.config.BindAddresses
	if addresses == nil {
		addresses = []string{""}
	}

	// ensure we could listen
	listeners := make([]net.Listener, 0)
	listenersHTTPS := make([]net.Listener, 0)
	promListeners := make([]net.Listener, 0)
	for _, addr := range addresses {
		if s.config.Domain != "" {
			ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", addr, 80))
			if err != nil {
				return err
			}
			listeners = append(listeners, ln)
			lnHTTPS, err := net.Listen("tcp", fmt.Sprintf("%s:%d", addr, 443))
			if err != nil {
				return err
			}
			listenersHTTPS = append(listenersHTTPS, lnHTTPS)
		} else {
			ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", addr, s.config.Port))
			if err != nil {
				return err
			}
			listeners = append(listeners, ln)
		}

		if s.promServer != nil {
			ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", addr, s.config.PrometheusPort))
			if err != nil {
				return err
			}
			promListeners = append(promListeners, ln)
		}
	}

	values := []interface{}{
		"portHttp", s.config.Port,
		"nodeID", s.currentNode.Id,
		"nodeIP", s.currentNode.Ip,
		"version", version.Version,
	}
	if s.config.BindAddresses != nil {
		values = append(values, "bindAddresses", s.config.BindAddresses)
	}
	if s.config.RTC.TCPPort != 0 {
		values = append(values, "rtc.portTCP", s.config.RTC.TCPPort)
	}
	if !s.config.RTC.ForceTCP && s.config.RTC.UDPPort != 0 {
		values = append(values, "rtc.portUDP", s.config.RTC.UDPPort)
	} else {
		values = append(values,
			"rtc.portICERange", []uint32{s.config.RTC.ICEPortRangeStart, s.config.RTC.ICEPortRangeEnd},
		)
	}
	if s.config.PrometheusPort != 0 {
		values = append(values, "portPrometheus", s.config.PrometheusPort)
	}
	if s.config.Region != "" {
		values = append(values, "region", s.config.Region)
	}
	logger.Infow("starting LiveKit server", values...)

	for _, promLn := range promListeners {
		go s.promServer.Serve(promLn)
	}

	if s.config.Domain != "" {
		httpGroup := &errgroup.Group{}
		for _, ln := range listeners {
			l := ln
			httpGroup.Go(func() error {
				return s.httpServer.Serve(l)
			})
		}
		for _, lnHTTPS := range listenersHTTPS {
			l := lnHTTPS
			httpGroup.Go(func() error {
				return s.httpsServer.ServeTLS(l, "", "")
			})
		}
		go func() {
			if err := httpGroup.Wait(); err != http.ErrServerClosed {
				logger.Errorw("could not start server", err)
				s.Stop(true)
			}
		}()
	} else {
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
	}

	go s.backgroundWorker()

	// give time for Serve goroutine to start
	time.Sleep(100 * time.Millisecond)

	s.running.Store(true)

	<-s.doneChan

	// wait for shutdown
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()
	_ = s.httpServer.Shutdown(ctx)

	if s.httpsServer != nil {
		_ = s.httpsServer.Shutdown(ctx)
	}

	if s.turnServer != nil {
		_ = s.turnServer.Close()
	}

	s.roomManager.Stop()
	s.signalServer.Stop()

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

	b, err := json.MarshalIndent(info, "", "\t")
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
	roomTicker := time.NewTicker(5 * time.Second)
	defer roomTicker.Stop()
	for {
		select {
		case <-s.doneChan:
			return
		case <-roomTicker.C:
			s.roomManager.CloseIdleRooms()
			s.roomManager.SaveClientsBandwidth()
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
