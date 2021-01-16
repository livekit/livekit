package main

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"os/signal"
	"runtime"
	"runtime/pprof"
	"syscall"
	"time"

	"github.com/urfave/cli/v2"

	"github.com/livekit/livekit-server/pkg/auth"
	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/logger"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/service"
	"github.com/livekit/livekit-server/pkg/utils"
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
				Usage: "when set, token validation will be disabled",
			},
		},
		Action: startServer,
		Commands: []*cli.Command{
			{
				Name:   "generate-keys",
				Usage:  "generates a pair of API & secret keys",
				Action: generateKeys,
			},
		},
	}

	if err := app.Run(os.Args); err != nil {
		fmt.Println(err)
	}
}

func startServer(c *cli.Context) error {
	rand.Seed(time.Now().UnixNano())

	cpuProfile := c.String("cpuprofile")
	memProfile := c.String("memprofile")
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
				pprof.WriteHeapProfile(f)
				f.Close()
			}()
		}
	}

	// require a key provider
	if keyProvider, err = createKeyProvider(c.String("key-file"), c.String("keys")); err != nil {
		return err
	}
	logger.Infow("auth enabled", "num_keys", keyProvider.NumKeys())

	currentNode, err := routing.NewLocalNode(conf)
	if err != nil {
		return err
	}

	// local routing and store
	router := routing.NewLocalRouter(currentNode)
	roomStore := service.NewLocalRoomStore()

	server, err := service.InitializeServer(conf, keyProvider,
		roomStore, router, currentNode)
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

func createKeyProvider(keyFile, keys string) (auth.KeyProvider, error) {
	// prefer keyfile if set
	if keyFile != "" {
		if st, err := os.Stat(keyFile); err != nil {
			return nil, err
		} else if st.Mode().Perm() != 0600 {
			return nil, fmt.Errorf("key file must have permission set to 600")
		}
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

func generateKeys(c *cli.Context) error {
	apiKey := utils.NewGuid(utils.APIKeyPrefix)
	secret := utils.RandomSecret()
	fmt.Println("API Key: ", apiKey)
	fmt.Println("Secret Key: ", secret)
	return nil
}
