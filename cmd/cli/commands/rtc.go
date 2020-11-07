package commands

import (
	"net/url"

	"github.com/gorilla/websocket"
	"github.com/urfave/cli/v2"

	"github.com/livekit/livekit-server/cmd/cli/client"
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
					Name: "token",
				},
				&cli.StringFlag{
					Name:  "peer-id",
					Value: "peer",
				},
			},
		},
	}
)

func joinRoom(c *cli.Context) error {
	u, err := url.Parse(c.String("host"))
	if err != nil {
		return err
	}

	v := url.Values{}
	v.Set("room_id", c.String("room-id"))
	v.Set("token", c.String("token"))
	v.Set("peer_id", c.String("peer-id"))
	u.RawQuery = v.Encode()

	logger.GetLogger().Infow("connecting to Websocket signal", "url", u.String())
	conn, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		return err
	}
	defer conn.Close()

	rc, err := client.NewRTCClient(conn)
	if err != nil {
		return err
	}

	// TODO: input loop to detect user commands

	return rc.Run()
}
