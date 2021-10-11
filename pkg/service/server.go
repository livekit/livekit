package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"runtime/pprof"
	"time"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/logger"
	livekit "github.com/livekit/protocol/proto"
	"github.com/livekit/protocol/utils"
	"github.com/pion/turn/v2"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/urfave/negroni"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/version"
)

type LivekitServer struct {
	config      *config.Config
	recService  *RecordingService
	rtcService  *RTCService
	httpServer  *http.Server
	promServer  *http.Server
	router      routing.Router
	roomManager *LocalRoomManager
	turnServer  *turn.Server
	currentNode routing.LocalNode
	running     utils.AtomicFlag
	doneChan    chan struct{}
	closedChan  chan struct{}
}

func NewLivekitServer(conf *config.Config,
	roomService livekit.RoomService,
	recService *RecordingService,
	rtcService *RTCService,
	keyProvider auth.KeyProvider,
	router routing.Router,
	roomManager *LocalRoomManager,
	turnServer *turn.Server,
	currentNode routing.LocalNode,
) (s *LivekitServer, err error) {
	s = &LivekitServer{
		config:      conf,
		recService:  recService,
		rtcService:  rtcService,
		router:      router,
		roomManager: roomManager,
		// turn server starts automatically
		turnServer:  turnServer,
		currentNode: currentNode,
		closedChan:  make(chan struct{}),
	}

	middlewares := []negroni.Handler{
		// always the first
		negroni.NewRecovery(),
	}
	if keyProvider != nil {
		middlewares = append(middlewares, NewAPIKeyAuthMiddleware(keyProvider))
	}

	roomServer := livekit.NewRoomServiceServer(roomService)
	recServer := livekit.NewRecordingServiceServer(recService)

	mux := http.NewServeMux()
	mux.Handle(roomServer.PathPrefix(), roomServer)
	mux.Handle(recServer.PathPrefix(), recServer)
	mux.Handle("/rtc", rtcService)
	mux.HandleFunc("/rtc/validate", rtcService.Validate)
	mux.HandleFunc("/", s.healthCheck)
	if conf.Development {
		mux.HandleFunc("/debug/goroutine", s.debugGoroutines)
		mux.HandleFunc("/debug/rooms", s.debugInfo)
	}

	s.httpServer = &http.Server{
		Addr:    fmt.Sprintf(":%d", conf.Port),
		Handler: configureMiddlewares(mux, middlewares...),
	}

	if conf.PrometheusPort > 0 {
		s.promServer = &http.Server{
			Addr:    fmt.Sprintf(":%d", conf.PrometheusPort),
			Handler: promhttp.Handler(),
		}
	}

	// clean up old rooms on startup
	if err = roomManager.CleanupRooms(); err != nil {
		return
	}
	if err = router.RemoveDeadNodes(); err != nil {
		return
	}

	return
}

func (s *LivekitServer) Node() *livekit.Node {
	return s.currentNode
}

func (s *LivekitServer) IsRunning() bool {
	return s.running.Get()
}

func (s *LivekitServer) Start() error {
	if s.running.Get() {
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

	s.recService.Start()

	// ensure we could listen
	ln, err := net.Listen("tcp", s.httpServer.Addr)
	if err != nil {
		return err
	}

	if s.promServer != nil {
		promLn, err := net.Listen("tcp", s.promServer.Addr)
		if err != nil {
			return err
		}
		go func() {
			_ = s.promServer.Serve(promLn)
		}()
	}

	go func() {
		values := []interface{}{
			"addr", s.httpServer.Addr,
			"nodeID", s.currentNode.Id,
			"nodeIP", s.currentNode.Ip,
			"version", version.Version,
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
		if err := s.httpServer.Serve(ln); err != http.ErrServerClosed {
			logger.Errorw("could not start server", err)
			s.Stop(true)
		}
	}()

	go s.backgroundWorker()

	// give time for Serve goroutine to start
	time.Sleep(10 * time.Millisecond)

	s.running.TrySet(true)

	<-s.doneChan

	// wait for shutdown
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()
	_ = s.httpServer.Shutdown(ctx)

	if s.turnServer != nil {
		_ = s.turnServer.Close()
	}

	s.roomManager.Stop()
	s.recService.Stop()

	close(s.closedChan)
	return nil
}

func (s *LivekitServer) Stop(force bool) {
	// wait for all participants to exit
	s.router.PreStop()
	partTicker := time.NewTicker(5 * time.Second)
	waitingForParticipants := !force && s.roomManager.HasParticipants()
	for waitingForParticipants {
		select {
		case <-partTicker.C:
			logger.Infow("waiting for participants to exit")
			waitingForParticipants = s.roomManager.HasParticipants()
		}
	}
	partTicker.Stop()

	if !s.running.TrySet(false) {
		return
	}

	s.router.Stop()
	close(s.doneChan)

	// wait for fully closed
	<-s.closedChan
}

func (s *LivekitServer) RoomManager() RoomManager {
	return s.roomManager
}

func (s *LivekitServer) debugGoroutines(w http.ResponseWriter, r *http.Request) {
	_ = pprof.Lookup("goroutine").WriteTo(w, 2)
}

func (s *LivekitServer) debugInfo(w http.ResponseWriter, r *http.Request) {
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

func (s *LivekitServer) healthCheck(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}

// worker to perform periodic tasks per node
func (s *LivekitServer) backgroundWorker() {
	roomTicker := time.NewTicker(30 * time.Second)
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
