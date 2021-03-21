package commands

import (
	"fmt"

	"github.com/urfave/cli/v2"

	"github.com/livekit/protocol/auth"
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
				&cli.BoolFlag{
					Name:  "admin",
					Usage: "enable token to be used to manage a room",
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
				&cli.StringFlag{
					Name:  "metadata",
					Usage: "JSON metadata to encode in the token, will be passed to participant",
				},
				devFlag,
			},
		},
	}
)

func createToken(c *cli.Context) error {
	if !c.IsSet("api-key") || !c.IsSet("api-secret") {
		return fmt.Errorf("api-key and api-secret are required")
	}
	p := c.String("participant") // required only for join
	room := c.String("room")
	metadata := c.String("metadata")

	grant := &auth.VideoGrant{}
	if c.Bool("create") {
		grant.RoomCreate = true
	}
	if c.Bool("join") {
		grant.RoomJoin = true
		grant.Room = room
		if p == "" {
			return fmt.Errorf("participant name is required")
		}
	}
	if c.Bool("admin") {
		grant.RoomAdmin = true
		grant.Room = room
	}

	if !grant.RoomJoin && !grant.RoomCreate && !grant.RoomAdmin {
		return fmt.Errorf("one of --join, --create, or --admin is required")
	}

	at := accessToken(c, grant, p)

	if metadata != "" {
		at.SetMetadata(metadata)
	}

	token, err := at.ToJWT()
	if err != nil {
		return err
	}

	fmt.Println("access token: ", token)
	return nil
}
