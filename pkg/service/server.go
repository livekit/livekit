package service

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/urfave/negroni"

	"github.com/livekit/livekit-server/pkg/auth"
	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/logger"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/utils"
	"github.com/livekit/livekit-server/proto/livekit"
	"github.com/livekit/livekit-server/version"
)

type LivekitServer struct {
	config      *config.Config
	roomServer  livekit.TwirpServer
	rtcService  *RTCService
	httpServer  *http.Server
	router      routing.Router
	roomManager *RoomManager
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
	currentNode routing.LocalNode,
) (s *LivekitServer, err error) {
	s = &LivekitServer{
		config:      conf,
		roomServer:  livekit.NewRoomServiceServer(roomService),
		rtcService:  rtcService,
		router:      router,
		roomManager: roomManager,
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

	s.httpServer = &http.Server{
		Addr:    fmt.Sprintf(":%d", conf.Port),
		Handler: configureMiddlewares(mux, middlewares...),
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
	defer s.router.UnregisterNode()

	if err := s.router.Start(); err != nil {
		return err
	}

	s.doneChan = make(chan bool, 1)

	// ensure we could listen
	ln, err := net.Listen("tcp", s.httpServer.Addr)
	if err != nil {
		return err
	}

	go func() {
		logger.Infow("starting LiveKit server", "address", s.httpServer.Addr,
			"nodeId", s.currentNode.Id,
			"version", version.Version)
		s.httpServer.Serve(ln)
	}()

	go s.backgroundWorker()

	s.running.TrySet(true)

	<-s.doneChan

	if err := s.router.UnregisterNode(); err != nil {
		logger.Errorw("could not unregister node", "error", err)
	}

	// wait for shutdown
	ctx, _ := context.WithTimeout(context.Background(), time.Second*5)
	s.httpServer.Shutdown(ctx)

	return nil
}

func (s *LivekitServer) Stop() {
	if s.running.TrySet(false) {
		s.router.Stop()
		close(s.doneChan)
	}
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
