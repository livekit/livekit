package service

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/pion/turn/v2"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/urfave/negroni"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/logger"
	"github.com/livekit/livekit-server/pkg/routing"
	livekit "github.com/livekit/livekit-server/proto"
	"github.com/livekit/livekit-server/version"
	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/utils"
)

type LivekitServer struct {
	config      *config.Config
	roomServer  livekit.TwirpServer
	rtcService  *RTCService
	httpServer  *http.Server
	promServer  *http.Server
	router      routing.Router
	roomManager *RoomManager
	turnServer  *turn.Server
	currentNode routing.LocalNode
	running     utils.AtomicFlag
	doneChan    chan bool
}

func NewLivekitServer(conf *config.Config,
	roomService livekit.RoomService,
	rtcService *RTCService,
	keyProvider auth.KeyProvider,
	router routing.Router,
	roomManager *RoomManager,
	turnServer *turn.Server,
	currentNode routing.LocalNode,
) (s *LivekitServer, err error) {
	s = &LivekitServer{
		config:      conf,
		roomServer:  livekit.NewRoomServiceServer(roomService),
		rtcService:  rtcService,
		router:      router,
		roomManager: roomManager,
		// turn server starts automatically
		turnServer:  turnServer,
		currentNode: currentNode,
	}

	middlewares := []negroni.Handler{
		// always the first
		negroni.NewRecovery(),
	}
	if keyProvider != nil {
		middlewares = append(middlewares, NewAPIKeyAuthMiddleware(keyProvider))
	}

	mux := http.NewServeMux()
	mux.Handle(s.roomServer.PathPrefix(), s.roomServer)
	mux.Handle("/rtc", rtcService)
	mux.HandleFunc("/", s.healthCheck)

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

	// hook up router to the RoomManager
	router.OnNewParticipantRTC(roomManager.StartSession)
	router.OnRTCMessage(roomManager.handleRTCMessage)

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

	s.doneChan = make(chan bool, 1)

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
			"address", s.httpServer.Addr,
			"nodeId", s.currentNode.Id,
			"version", version.Version,
		}
		if s.config.RTC.TCPPort != 0 {
			values = append(values, "rtc.tcp_port", s.config.RTC.TCPPort)
		}
		if !s.config.RTC.ForceTCP && s.config.RTC.UDPPort != 0 {
			values = append(values, "rtc.udp_port", s.config.RTC.UDPPort)
		} else {
			values = append(values,
				"rtc.port_range_start", s.config.RTC.ICEPortRangeStart,
				"rtc.port_range_end", s.config.RTC.ICEPortRangeEnd,
			)
		}
		if s.config.PrometheusPort != 0 {
			values = append(values, "prometheus_port", s.config.PrometheusPort)
		}
		logger.Infow("starting LiveKit server", values...)
		if err := s.httpServer.Serve(ln); err != http.ErrServerClosed {
			logger.Errorw("could not start server", err)
			s.Stop()
		}
	}()

	go s.backgroundWorker()

	s.running.TrySet(true)

	<-s.doneChan

	if err := s.router.UnregisterNode(); err != nil {
		logger.Errorw("could not unregister node", err)
	}

	// wait for shutdown
	ctx, _ := context.WithTimeout(context.Background(), time.Second*5)
	_ = s.httpServer.Shutdown(ctx)

	if s.turnServer != nil {
		_ = s.turnServer.Close()
	}

	s.roomManager.Stop()

	return nil
}

func (s *LivekitServer) Stop() {
	if !s.running.TrySet(false) {
		return
	}

	s.router.Stop()
	s.roomManager.Stop()
	close(s.doneChan)
}

func (s *LivekitServer) RoomManager() *RoomManager {
	return s.roomManager
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
