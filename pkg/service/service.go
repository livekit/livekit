package service

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/google/wire"
	"github.com/urfave/negroni"

	"github.com/livekit/livekit-server/pkg/auth"
	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/logger"
	"github.com/livekit/livekit-server/pkg/node"
	"github.com/livekit/livekit-server/pkg/rtc"
	"github.com/livekit/livekit-server/proto/livekit"
)

var ServiceSet = wire.NewSet(
	NewRoomService,
	NewRTCService,
	NewLivekitServer,
	newRoomManagerWithNode,
)

func NewRoomService(conf *config.Config, manager *rtc.RoomManager, localNode *node.Node) (livekit.RoomService, error) {
	if conf.MultiNode {
		return nil, fmt.Errorf("multinode is not supported")
	} else {
		return NewSimpleRoomService(manager, localNode)
	}
}

type LivekitServer struct {
	config     *config.Config
	roomServer livekit.TwirpServer
	rtcService *RTCService
	roomHttp   *http.Server
	rtcHttp    *http.Server
	running    bool
	doneChan   chan bool
}

func newRoomManagerWithNode(conf *config.Config, localNode *node.Node) (*rtc.RoomManager, error) {
	return rtc.NewRoomManager(conf.RTC, localNode.Ip)
}

func NewLivekitServer(conf *config.Config,
	roomService livekit.RoomService,
	rtcService *RTCService,
	keyProvider auth.KeyProvider) (s *LivekitServer, err error) {
	s = &LivekitServer{
		config:     conf,
		roomServer: livekit.NewRoomServiceServer(roomService),
		rtcService: rtcService,
	}

	middlewares := make([]negroni.Handler, 0)
	if keyProvider != nil {
		middlewares = append(middlewares, NewAPIKeyAuthMiddleware(keyProvider))
	}

	s.roomHttp = &http.Server{
		Addr:    fmt.Sprintf(":%d", conf.APIPort),
		Handler: configureMiddlewares(s.roomServer, middlewares...),
	}

	rtcHandler := http.NewServeMux()
	rtcHandler.Handle("/rtc", rtcService)
	s.rtcHttp = &http.Server{
		Addr:    fmt.Sprintf(":%d", conf.RTCPort),
		Handler: configureMiddlewares(rtcHandler, middlewares...),
	}

	return
}

func (s *LivekitServer) IsRunning() bool {
	return s.running
}

func (s *LivekitServer) Start() error {
	if s.running {
		return errors.New("already running")
	}
	s.doneChan = make(chan bool, 1)

	// ensure we could listen
	roomLn, err := net.Listen("tcp", s.roomHttp.Addr)
	if err != nil {
		return err
	}

	rtcAddr := fmt.Sprintf(":%d", s.config.RTCPort)
	rtcLn, err := net.Listen("tcp", rtcAddr)
	if err != nil {
		return err
	}

	go func() {
		logger.GetLogger().Infow("starting Room service", "address", s.roomHttp.Addr)
		s.roomHttp.Serve(roomLn)
	}()
	go func() {
		logger.GetLogger().Infow("starting RTC service", "address", rtcAddr)
		s.rtcHttp.Serve(rtcLn)
	}()

	s.running = true

	<-s.doneChan

	// wait for shutdown
	ctx, _ := context.WithTimeout(context.Background(), time.Second*5)
	wg := sync.WaitGroup{}
	wg.Add(2)
	go func() {
		defer wg.Done()
		s.rtcHttp.Shutdown(ctx)
	}()
	go func() {
		defer wg.Done()
		s.roomHttp.Shutdown(ctx)
	}()
	wg.Wait()

	return nil
}

func (s *LivekitServer) Stop() {
	s.running = false
	s.doneChan <- true
}

func configureMiddlewares(handler http.Handler, middlewares ...negroni.Handler) *negroni.Negroni {
	n := negroni.New()
	n.Use(negroni.NewRecovery())
	for _, m := range middlewares {
		n.Use(m)
	}
	n.UseHandler(handler)
	return n
}
