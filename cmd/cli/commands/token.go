package commands

import (
	"fmt"

	"github.com/urfave/cli/v2"

	"github.com/livekit/livekit-server/pkg/auth"
)

var (
	TokenCommands = []*cli.Command{
		{
			Name:   "create-token",
			Usage:  "create token for Room APIs",
			Action: createToken,
			Flags: []cli.Flag{
				apiKeyFlag,
				secretFlag,
				&cli.BoolFlag{
					Name:  "join",
					Usage: "enable token to be used to join a room",
				},
				&cli.BoolFlag{
					Name:  "create",
					Usage: "enable token to be used to create rooms",
				},
				&cli.StringFlag{
					Name:    "participant",
					Aliases: []string{"p"},
					Usage:   "unique name of the participant, used with --join",
				},
				&cli.StringFlag{
					Name:    "room",
					Aliases: []string{"r"},
					Usage:   "name of the room to join, empty to allow joining all rooms",
				},
			},
		},
	}
)

func createToken(c *cli.Context) error {
	if !c.IsSet("api-key") || !c.IsSet("api-secret") {
		return fmt.Errorf("api-key and api-secret are required")
	}
	p := c.String("participant") // required only for join

	grant := &auth.VideoGrant{}
	if c.Bool("create") {
		grant.RoomCreate = true
	}
	if c.Bool("join") {
		grant.RoomJoin = true

		if room := c.String("room"); room != "" {
			grant.Room = room
		}

		if p == "" {
			return fmt.Errorf("participant name is required")
		}
	}

	if !grant.RoomJoin && !grant.RoomCreate {
		return fmt.Errorf("one of --join or --create is required")
	}

	token, err := accessToken(c, grant, p)
	if err != nil {
		return err
	}

	fmt.Println("access token: ", token)
	return nil
}
