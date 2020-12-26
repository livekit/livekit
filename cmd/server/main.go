package main

import (
	"bytes"
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

	"github.com/livekit/livekit-server/pkg/auth"
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
				Usage:   "path to LiveKit config",
				EnvVars: []string{"LIVEKIT_CONFIG"},
			},
			&cli.StringFlag{
				Name:    "key-file",
				Usage:   "path to file that contains API keys/secrets",
				EnvVars: []string{"KEY_FILE"},
			},
			&cli.StringFlag{
				Name:    "keys",
				Usage:   "API keys/secret pairs (key:secret, one per line)",
				EnvVars: []string{"KEY_FILE"},
			},
			&cli.BoolFlag{
				Name:  "dev",
				Usage: "when set, token validation will be disabled",
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
	var keyProvider auth.KeyProvider

	if conf.Development {
		logger.InitDevelopment()
	} else {
		logger.InitProduction()
		// require a key provider
		if keyProvider, err = createKeyProvider(c.String("key-file"), c.String("keys")); err != nil {
			return err
		}
		service.AuthRequired = true
	}

	server, err := InitializeServer(conf, keyProvider)
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

func createKeyProvider(keyFile, keys string) (auth.KeyProvider, error) {
	// prefer keyfile if set
	if keyFile != "" {
		f, err := os.Open(keyFile)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		return auth.NewFileBasedKeyProvider(f)
	}

	if keys != "" {
		r := bytes.NewReader([]byte(keys))
		return auth.NewFileBasedKeyProvider(r)
	}

	return nil, errors.New("one of key-file or keys must be provided in order to support a secure installation")
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
	rtcService *service.RTCService,
	keyProvider auth.KeyProvider) (s *LivekitServer, err error) {
	s = &LivekitServer{
		config:     conf,
		roomServer: livekit.NewRoomServiceServer(roomService),
		rtcService: rtcService,
	}

	middlewares := make([]negroni.Handler, 0)
	if keyProvider != nil {
		middlewares = append(middlewares, service.NewAPIKeyAuthMiddleware(keyProvider))
	}

	s.roomHttp = &http.Server{
		Addr:    fmt.Sprintf(":%d", conf.APIPort),
		Handler: configureMiddlewares(conf, s.roomServer, middlewares...),
	}

	rtcHandler := http.NewServeMux()
	rtcHandler.Handle("/rtc", rtcService)
	s.rtcHttp = &http.Server{
		Addr:    fmt.Sprintf(":%d", conf.RTCPort),
		Handler: configureMiddlewares(conf, rtcHandler, middlewares...),
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

func configureMiddlewares(conf *config.Config, handler http.Handler, middlewares ...negroni.Handler) *negroni.Negroni {
	n := negroni.New()
	n.Use(negroni.NewRecovery())
	for _, m := range middlewares {
		n.Use(m)
	}
	n.UseHandler(handler)
	return n
}

func newManager(conf *config.Config, localNode *node.Node) (*rtc.RoomManager, error) {
	return rtc.NewRoomManager(conf.RTC, localNode.Ip)
}
