package commands

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/urfave/cli/v2"

	"github.com/livekit/livekit-server/cmd/cli/client"
	"github.com/livekit/livekit-server/pkg/auth"
	"github.com/livekit/livekit-server/pkg/logger"
)

var (
	RTCCommands = []*cli.Command{
		{
			Name:   "join",
			Action: joinRoom,
			Flags: []cli.Flag{
				rtcHostFlag,
				&cli.StringFlag{
					Name:  "token",
					Usage: "access token, not required in dev mode. if passed in, ignores --api-key, --api-secret, and --identity",
				},
				&cli.StringFlag{
					Name:  "identity",
					Usage: "identity of the participant, required if token isn't passed in",
				},
				&cli.StringFlag{
					Name:  "audio",
					Usage: "an ogg file to publish upon connection",
				},
				&cli.StringFlag{
					Name:  "video",
					Usage: "an ivf file to publish upon connection",
				},
				apiKeyFlag,
				secretFlag,
			},
		},
	}
)

func joinRoom(c *cli.Context) error {
	identity := c.String("identity")
	roomId := c.String("room")
	token := c.String("token")

	// generate access token if needed
	if token == "" {
		// require roomId & name to be passed in
		if roomId == "" {
			return fmt.Errorf("--room is required")
		}
		if identity == "" {
			return fmt.Errorf("--identity is required")
		}
		// token may be nil in dev mode
		var err error
		token, err = accessToken(c, &auth.VideoGrant{
			RoomJoin: true,
			Room:     roomId,
		}, identity).ToJWT()
		if err != nil {
			return err
		}
	}

	host := c.String("host")
	logger.Infow("connecting to Websocket signal", "host", host)
	conn, err := client.NewWebSocketConn(host, token)
	if err != nil {
		return err
	}
	defer conn.Close()

	rc, err := client.NewRTCClient(conn)
	if err != nil {
		return err
	}

	handleSignals(rc)

	// add tracks if needed
	audioFile := c.String("audio")
	videoFile := c.String("video")
	rc.OnConnected = func() {
		// add after connection, since we need proper publish track APIs
		if audioFile != "" {
			rc.AddFileTrack(audioFile, "audio", filepath.Base(audioFile))
		}
		if videoFile != "" {
			rc.AddFileTrack(videoFile, "video", filepath.Base(videoFile))
		}
	}

	return rc.Run()
}

func handleSignals(rc *client.RTCClient) {
	// signal to stop client
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)

	go func() {
		sig := <-sigChan
		logger.Infow("exit requested, shutting down", "signal", sig)
		rc.Stop()
	}()
}
