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
	"github.com/livekit/livekit-server/proto/livekit"
)

type LivekitServer struct {
	config      *config.Config
	roomServer  livekit.TwirpServer
	rtcService  *RTCService
	httpServer  *http.Server
	router      routing.Router
	currentNode routing.LocalNode
	running     bool
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

	// clean up old rooms on startup
	err = roomManager.Cleanup()

	return
}

func (s *LivekitServer) IsRunning() bool {
	return s.running
}

func (s *LivekitServer) Start() error {
	if s.running {
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
			"nodeId", s.currentNode.Id)
		s.httpServer.Serve(ln)
	}()

	s.running = true

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
	s.running = false
	s.router.Stop()
	s.doneChan <- true
}

func configureMiddlewares(handler http.Handler, middlewares ...negroni.Handler) *negroni.Negroni {
	n := negroni.New()
	for _, m := range middlewares {
		n.Use(m)
	}
	n.UseHandler(handler)
	return n
}
