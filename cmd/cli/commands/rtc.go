package commands

import (
	"net/url"

	"github.com/gorilla/websocket"
	"github.com/urfave/cli/v2"
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
					Name:     "token",
					Required: true,
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
	v.Set("roomId", c.String("room-id"))
	v.Set("token", c.String("token"))
	v.Set("peerId", c.String("peer-id"))
	u.RawQuery = v.Encode()

	conn, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		return err
	}
	defer conn.Close()

	return nil
}
