package main

import (
	"fmt"
	"math/rand"
	"os"
	"os/signal"
	"runtime"
	"runtime/pprof"
	"syscall"
	"time"

	"github.com/urfave/cli/v2"

	"github.com/livekit/livekit-server/pkg/rtc"
	"github.com/livekit/livekit-server/pkg/telemetry/prometheus"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/service"
	"github.com/livekit/livekit-server/version"
)

var baseFlags = []cli.Flag{
	&cli.StringSliceFlag{
		Name:  "bind",
		Usage: "IP address to listen on, use flag multiple times to specify multiple addresses",
	},
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
		Name:    "region",
		Usage:   "region of the current node. Used by regionaware node selector",
		EnvVars: []string{"LIVEKIT_REGION"},
	},
	&cli.StringFlag{
		Name:    "node-ip",
		Usage:   "IP address of the current node, used to advertise to clients. Automatically determined by default",
		EnvVars: []string{"NODE_IP"},
	},
	&cli.IntFlag{
		Name:    "udp-port",
		Usage:   "Single UDP port to use for WebRTC traffic",
		EnvVars: []string{"UDP_PORT"},
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
		Name:    "turn-cert",
		Usage:   "tls cert file for TURN server",
		EnvVars: []string{"LIVEKIT_TURN_CERT"},
	},
	&cli.StringFlag{
		Name:    "turn-key",
		Usage:   "tls key file for TURN server",
		EnvVars: []string{"LIVEKIT_TURN_KEY"},
	},
	// debugging flags
	&cli.StringFlag{
		Name:  "memprofile",
		Usage: "write memory profile to `file`",
	},
	&cli.BoolFlag{
		Name:  "dev",
		Usage: "sets log-level to debug, console formatter, and /debug/pprof. insecure for production",
	},
	&cli.BoolFlag{
		Name:   "disable-strict-config",
		Usage:  "disables strict config parsing",
		Hidden: true,
	},
}

func init() {
	rand.Seed(time.Now().Unix())
}

func main() {
	defer func() {
		if rtc.Recover(logger.GetLogger()) != nil {
			os.Exit(1)
		}
	}()

	generatedFlags, err := config.GenerateCLIFlags(baseFlags, true)
	if err != nil {
		fmt.Println(err)
	}

	app := &cli.App{
		Name:        "livekit-server",
		Usage:       "High performance WebRTC server",
		Description: "run without subcommands to start the server",
		Flags:       append(baseFlags, generatedFlags...),
		Action:      startServer,
		Commands: []*cli.Command{
			{
				Name:   "generate-keys",
				Usage:  "generates an API key and secret pair",
				Action: generateKeys,
			},
			{
				Name:   "ports",
				Usage:  "print ports that server is configured to use",
				Action: printPorts,
			},
			{
				// this subcommand is deprecated, token generation is provided by CLI
				Name:   "create-join-token",
				Hidden: true,
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
					&cli.BoolFlag{
						Name:     "recorder",
						Usage:    "creates a hidden participant that can only subscribe",
						Required: false,
					},
				},
			},
			{
				Name:   "list-nodes",
				Usage:  "list all nodes",
				Action: listNodes,
			},
			{
				Name:   "help-verbose",
				Usage:  "prints app help, including all generated configuration flags",
				Action: helpVerbose,
			},
		},
		Version: version.Version,
	}

	if err := app.Run(os.Args); err != nil {
		fmt.Println(err)
	}
}

func getConfig(c *cli.Context) (*config.Config, error) {
	confString, err := getConfigString(c.String("config"), c.String("config-body"))
	if err != nil {
		return nil, err
	}

	strictMode := true
	if c.Bool("disable-strict-config") {
		strictMode = false
	}

	conf, err := config.NewConfig(confString, strictMode, c, baseFlags)
	if err != nil {
		return nil, err
	}
	config.InitLoggerFromConfig(conf.Logging)

	if c.String("config") == "" && c.String("config-body") == "" && conf.Development {
		// use single port UDP when no config is provided
		conf.RTC.UDPPort = 7882
		conf.RTC.ICEPortRangeStart = 0
		conf.RTC.ICEPortRangeEnd = 0
		logger.Infow("starting in development mode")

		if len(conf.Keys) == 0 {
			logger.Infow("no keys provided, using placeholder keys",
				"API Key", "devkey",
				"API Secret", "secret",
			)
			conf.Keys = map[string]string{
				"devkey": "secret",
			}
			// when dev mode and using shared keys, we'll bind to localhost by default
			if conf.BindAddresses == nil {
				conf.BindAddresses = []string{
					"127.0.0.1",
					"[::1]",
				}
			}
		}
	}
	return conf, nil
}

func startServer(c *cli.Context) error {
	rand.Seed(time.Now().UnixNano())

	memProfile := c.String("memprofile")

	conf, err := getConfig(c)
	if err != nil {
		return err
	}

	// validate API key length
	err = conf.ValidateKeys()
	if err != nil {
		return err
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

	currentNode, err := routing.NewLocalNode(conf)
	if err != nil {
		return err
	}

	prometheus.Init(currentNode.Id, currentNode.Type, conf.Environment)

	server, err := service.InitializeServer(conf, currentNode)
	if err != nil {
		return err
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)

	go func() {
		sig := <-sigChan
		logger.Infow("exit requested, shutting down", "signal", sig)
		server.Stop(false)
	}()

	return server.Start()
}

func getConfigString(configFile string, inConfigBody string) (string, error) {
	if inConfigBody != "" || configFile == "" {
		return inConfigBody, nil
	}

	outConfigBody, err := os.ReadFile(configFile)
	if err != nil {
		return "", err
	}

	return string(outConfigBody), nil
}
