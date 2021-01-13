package commands

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/manifoldco/promptui"
	"github.com/pion/webrtc/v3"
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
				roomFlag,
				rtcHostFlag,
				&cli.StringFlag{
					Name:  "token",
					Usage: "access token, not required in dev mode. if passed in, ignores --api-key, --api-secret, and --name",
				},
				&cli.StringFlag{
					Name:  "name",
					Usage: "name of participant",
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
	name := c.String("name")
	roomId := c.String("room")
	token := c.String("token")

	// generate access token if needed
	if token == "" {
		// require roomId & name to be passed in
		if roomId == "" {
			return fmt.Errorf("--room is required")
		}
		if name == "" {
			return fmt.Errorf("--name is required")
		}
		// token may be nil in dev mode
		var err error
		token, err = accessToken(c, &auth.VideoGrant{
			RoomJoin: true,
			Room:     roomId,
		}, name)
		if err != nil {
			return err
		}
	}

	log := logger.GetLogger()

	host := c.String("host")
	log.Infow("connecting to Websocket signal", "host", host)
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
			rc.AddTrack(audioFile, "audio", filepath.Base(audioFile))
		}
		if videoFile != "" {
			rc.AddTrack(videoFile, "video", filepath.Base(videoFile))
		}
	}

	// start loop to detect input
	go func() {
		for {
			r := bufio.NewReader(os.Stdin)
			_, _, err := r.ReadLine()
			if err != nil {
				return
			}

			// pause client output and wait
			rc.PauseLogs()

			err = handleCommand(rc)
			if err != nil {
				log.Errorw("could not handle command", "err", err)
			}

			rc.ResumeLogs()
		}
	}()

	return rc.Run()
}

func handleCommand(rc *client.RTCClient) error {
	cmdPrompt := promptui.Select{
		Label: "select command",
		Items: []string{
			"add video",
			"add audio",
			//"add data",
			//"send data",
			"remove track",
		},
	}

	idx, _, err := cmdPrompt.Run()
	if err != nil {
		return err
	}

	switch idx {
	case 0:
		return handleAddMedia(rc, false)
	case 1:
		return handleAddMedia(rc, true)
	default:
		return errors.New("unimplemented command")
	}
	return nil
}

func handleAddMedia(rc *client.RTCClient, isAudio bool) error {
	fileType := "vp8, h264"
	codecType := webrtc.RTPCodecTypeVideo
	if isAudio {
		fileType = "opus"
		codecType = webrtc.RTPCodecTypeAudio
	}
	// get media location
	p := promptui.Prompt{
		Label: fmt.Sprintf("media path (%s)", fileType),
		Validate: func(s string) error {
			s = ExpandUser(s)
			// ensure it exists
			st, err := os.Stat(s)
			if err != nil {
				return err
			}
			if st.IsDir() {
				return errors.New("cannot be a directory")
			}
			return nil
		},
	}

	mediaPath, err := p.Run()
	if err != nil {
		return err
	}

	mediaPath = ExpandUser(mediaPath)

	// TODO: see what the ID should be
	err = rc.AddTrack(mediaPath, codecType.String(), filepath.Base(mediaPath))
	if err != nil {
		return err
	}
	fmt.Printf("added %s track: %s\n", codecType.String(), mediaPath)

	if isAudio {
		return nil
	}

	// for video files, also look for the .ogg at the same path
	videoExt := filepath.Ext(mediaPath)
	if len(videoExt) == 0 {
		return nil
	}

	audioPath := mediaPath[0:len(mediaPath)-len(videoExt)] + ".ogg"
	if _, err = os.Stat(audioPath); err == nil {
		err = rc.AddTrack(audioPath, codecType.String(), filepath.Base(audioPath))
		if err != nil {
			fmt.Printf("added audio track: %s\n", audioPath)
		}
	}
	return err
}

func handleSignals(rc *client.RTCClient) {
	// signal to stop client
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)

	go func() {
		sig := <-sigChan
		logger.GetLogger().Infow("exit requested, shutting down", "signal", sig)
		rc.Stop()
	}()
}
