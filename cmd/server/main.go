package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/urfave/cli/v2"
	"github.com/urfave/negroni"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/logger"
	"github.com/livekit/livekit-server/pkg/node"
	"github.com/livekit/livekit-server/pkg/rtc"
	"github.com/livekit/livekit-server/pkg/service"
	"github.com/livekit/livekit-server/proto/livekit"
)

func main() {
	app := &cli.App{
		Name: "livekit-server",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "config",
				EnvVars: []string{"LIVEKIT_CONFIG"},
			},
			&cli.BoolFlag{
				Name: "dev",
			},
		},
		Action: startServer,
	}

	if err := app.Run(os.Args); err != nil {
		fmt.Println(err)
	}
}

func startServer(c *cli.Context) error {
	conf, err := config.NewConfig(c.String("config"))
	if err != nil {
		return err
	}

	conf.UpdateFromCLI(c)

	if conf.Development {
		logger.InitDevelopment()
	} else {
		logger.InitProduction()
	}

	server, err := InitializeServer(conf)
	if err != nil {
		return err
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)

	go func() {
		sig := <-sigChan
		logger.GetLogger().Infow("exit requested, shutting down", "signal", sig)
		server.Stop()
	}()

	return server.Start()
}

type LivekitServer struct {
	config     *config.Config
	roomServer livekit.TwirpServer
	rtcService *service.RTCService
	roomHttp   *http.Server
	rtcHttp    *http.Server
	running    bool
	doneChan   chan bool
}

func NewLivekitServer(conf *config.Config,
	roomService livekit.RoomService,
	rtcService *service.RTCService) (s *LivekitServer, err error) {
	s = &LivekitServer{
		config:     conf,
		roomServer: livekit.NewRoomServiceServer(roomService),
		rtcService: rtcService,
	}

	roomHandler := configureMiddlewares(conf, s.roomServer)
	s.roomHttp = &http.Server{
		Addr:    fmt.Sprintf(":%d", conf.APIPort),
		Handler: roomHandler,
	}

	rtcHandler := http.NewServeMux()
	rtcHandler.Handle("/rtc", rtcService)
	s.rtcHttp = &http.Server{
		Addr:    fmt.Sprintf(":%d", conf.RTCPort),
		Handler: configureMiddlewares(conf, rtcHandler),
	}

	return
}

func (s *LivekitServer) Start() error {
	if s.running {
		return errors.New("already running")
	}
	s.running = true
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

func configureMiddlewares(conf *config.Config, handler http.Handler) *negroni.Negroni {
	n := negroni.New()
	n.Use(negroni.NewRecovery())
	n.UseHandler(handler)
	return n
}

func newManager(conf *config.Config, localNode *node.Node) (*rtc.RoomManager, error) {
	return rtc.NewRoomManager(conf.RTC, localNode.Ip)
}
