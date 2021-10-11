package main

import (
	"fmt"
	"io/ioutil"
	"math/rand"
	"os"
	"os/signal"
	"runtime"
	"runtime/pprof"
	"syscall"
	"time"

	"github.com/livekit/protocol/logger"
	"github.com/urfave/cli/v2"

	"github.com/livekit/livekit-server/pkg/config"
	serverlogger "github.com/livekit/livekit-server/pkg/logger"
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
				Name:    "region",
				Usage:   "region of the current node. Used by regionaware node selector",
				EnvVars: []string{"LIVEKIT_REGION"},
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
				Usage:  "generates an API key and secret pair",
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
		serverlogger.InitDevelopment(conf.LogLevel)
	} else {
		serverlogger.InitProduction(conf.LogLevel)
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

	currentNode, err := routing.NewLocalNode(conf)
	if err != nil {
		return err
	}

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

	outConfigBody, err := ioutil.ReadFile(configFile)
	if err != nil {
		return "", err
	}

	return string(outConfigBody), nil
}
