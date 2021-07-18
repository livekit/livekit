package main

import (
	"context"
	"fmt"
	"io/ioutil"
	"math/rand"
	"os"
	"os/signal"
	"runtime"
	"runtime/pprof"
	"syscall"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/pkg/errors"
	"github.com/urfave/cli/v2"

	"github.com/livekit/protocol/auth"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/logger"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/service"
	"github.com/livekit/livekit-server/version"
)

func init() {
	rand.Seed(time.Now().Unix())
}

func main() {
	app := &cli.App{
		Name:        "livekit-server",
		Usage:       "distributed audio/video rooms over WebRTC",
		Description: "run without subcommands to start the server",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  "config",
				Usage: "path to LiveKit config file",
			},
			&cli.StringFlag{
				Name:    "config-body",
				Usage:   "LiveKit config in YAML, typically passed in as an environment var in a container",
				EnvVars: []string{"LIVEKIT_CONFIG"},
			},
			&cli.StringFlag{
				Name:  "key-file",
				Usage: "path to file that contains API keys/secrets",
			},
			&cli.StringFlag{
				Name:    "keys",
				Usage:   "api keys (key: secret\\n)",
				EnvVars: []string{"LIVEKIT_KEYS"},
			},
			&cli.StringFlag{
				Name:    "node-ip",
				Usage:   "IP address of the current node, used to advertise to clients. Automatically determined by default",
				EnvVars: []string{"NODE_IP"},
			},
			&cli.StringFlag{
				Name:    "redis-host",
				Usage:   "host (incl. port) to redis server",
				EnvVars: []string{"REDIS_HOST"},
			},
			&cli.StringFlag{
				Name:    "redis-password",
				Usage:   "password to redis",
				EnvVars: []string{"REDIS_PASSWORD"},
			},
			&cli.StringFlag{
				Name:  "cpuprofile",
				Usage: "write cpu profile to `file`",
			},
			&cli.StringFlag{
				Name:  "memprofile",
				Usage: "write memory profile to `file`",
			},
			&cli.BoolFlag{
				Name:  "dev",
				Usage: "sets log-level to debug, and console formatter",
			},
			&cli.StringFlag{
				Name:    "turn-cert",
				Usage:   "tls cert file for TURN server",
				EnvVars: []string{"LIVEKIT_TURN_CERT"},
			},
			&cli.StringFlag{
				Name:    "turn-key",
				Usage:   "tls key file for TURN server",
				EnvVars: []string{"LIVEKIT_TURN_KEY"},
			},
		},
		Action: startServer,
		Commands: []*cli.Command{
			{
				Name:   "generate-keys",
				Usage:  "generates a pair of API & secret keys",
				Action: generateKeys,
			},
			{
				Name:   "ports",
				Usage:  "print ports that server is configured to use",
				Action: printPorts,
			},
			{
				Name:   "create-join-token",
				Usage:  "create a room join token for development use",
				Action: createToken,
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:     "room",
						Usage:    "name of room to join",
						Required: true,
					},
					&cli.StringFlag{
						Name:     "identity",
						Usage:    "identity of participant that holds the token",
						Required: true,
					},
				},
			},
		},
		Version: version.Version,
	}

	if err := app.Run(os.Args); err != nil {
		fmt.Println(err)
	}
}

func getConfig(c *cli.Context) (*config.Config, error) {
	confString, err := getConfigString(c)
	if err != nil {
		return nil, err
	}

	return config.NewConfig(confString, c)
}

func startServer(c *cli.Context) error {
	rand.Seed(time.Now().UnixNano())

	cpuProfile := c.String("cpuprofile")
	memProfile := c.String("memprofile")

	conf, err := getConfig(c)
	if err != nil {
		return err
	}

	if conf.Development {
		logger.InitDevelopment(conf.LogLevel)
	} else {
		logger.InitProduction(conf.LogLevel)
	}

	if cpuProfile != "" {
		if f, err := os.Create(cpuProfile); err != nil {
			return err
		} else {
			defer f.Close()
			if err := pprof.StartCPUProfile(f); err != nil {
				return err
			}
			defer pprof.StopCPUProfile()
		}
	}

	if memProfile != "" {
		if f, err := os.Create(memProfile); err != nil {
			return err
		} else {
			defer func() {
				// run memory profile at termination
				runtime.GC()
				_ = pprof.WriteHeapProfile(f)
				_ = f.Close()
			}()
		}
	}

	// require a key provider
	keyProvider, err := createKeyProvider(conf)
	if err != nil {
		return err
	}
	logger.Infow("configured key provider", "numKeys", keyProvider.NumKeys())

	currentNode, err := routing.NewLocalNode(conf)
	if err != nil {
		return err
	}

	// local routing and store
	router, roomStore, err := createRouterAndStore(conf, currentNode)
	if err != nil {
		return err
	}

	server, err := service.InitializeServer(conf, keyProvider,
		roomStore, router, currentNode, &routing.RandomSelector{})
	if err != nil {
		return err
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)

	go func() {
		sig := <-sigChan
		logger.Infow("exit requested, shutting down", "signal", sig)
		server.Stop()
	}()

	return server.Start()
}

func createRouterAndStore(config *config.Config, node routing.LocalNode) (router routing.Router, store service.RoomStore, err error) {
	if config.HasRedis() {
		logger.Infow("using multi-node routing via redis", "addr", config.Redis.Address)
		rc := redis.NewClient(&redis.Options{
			Addr:     config.Redis.Address,
			Username: config.Redis.Username,
			Password: config.Redis.Password,
			DB:       config.Redis.DB,
		})
		if err = rc.Ping(context.Background()).Err(); err != nil {
			err = errors.Wrap(err, "unable to connect to redis")
			return
		}

		router = routing.NewRedisRouter(node, rc)
		store = service.NewRedisRoomStore(rc)
	} else {
		// local routing and store
		logger.Infow("using single-node routing")
		router = routing.NewLocalRouter(node)
		store = service.NewLocalRoomStore()
	}
	return
}

func createKeyProvider(conf *config.Config) (auth.KeyProvider, error) {
	// prefer keyfile if set
	if conf.KeyFile != "" {
		if st, err := os.Stat(conf.KeyFile); err != nil {
			return nil, err
		} else if st.Mode().Perm() != 0600 {
			return nil, fmt.Errorf("key file must have permission set to 600")
		}
		f, err := os.Open(conf.KeyFile)
		if err != nil {
			return nil, err
		}
		defer func() {
			_ = f.Close()
		}()
		return auth.NewFileBasedKeyProviderFromReader(f)
	}

	if len(conf.Keys) == 0 {
		return nil, errors.New("one of key-file or keys must be provided in order to support a secure installation")
	}

	return auth.NewFileBasedKeyProviderFromMap(conf.Keys), nil
}

func getConfigString(c *cli.Context) (string, error) {
	configFile := c.String("config")
	configBody := c.String("config-body")
	if configBody == "" {
		if configFile != "" {
			content, err := ioutil.ReadFile(configFile)
			if err != nil {
				return "", err
			}
			configBody = string(content)
		}
	}
	return configBody, nil
}
