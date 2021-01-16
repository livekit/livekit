package commands

import (
	"context"
	"fmt"
	"net/http"

	"github.com/twitchtv/twirp"
	"github.com/urfave/cli/v2"

	"github.com/livekit/livekit-server/pkg/auth"
	"github.com/livekit/livekit-server/pkg/logger"
	"github.com/livekit/livekit-server/proto/livekit"
)

var (
	RoomCommands = []*cli.Command{
		{
			Name:   "create-room",
			Before: createClient,
			Action: createRoom,
			Flags: []cli.Flag{
				roomHostFlag,
				&cli.StringFlag{
					Name:     "name",
					Usage:    "name of the room",
					Required: true,
				},
				apiKeyFlag,
				secretFlag,
			},
		},
		{
			Name:   "list-rooms",
			Before: createClient,
			Action: listRooms,
			Flags: []cli.Flag{
				roomHostFlag,
				apiKeyFlag,
				secretFlag,
			},
		},
		{
			Name:   "delete-room",
			Before: createClient,
			Action: deleteRoom,
			Flags: []cli.Flag{
				roomFlag,
				roomHostFlag,
				apiKeyFlag,
				secretFlag,
			},
		},
	}

	roomClient livekit.RoomService
)

func createClient(c *cli.Context) error {
	host := c.String("host")
	roomClient = livekit.NewRoomServiceJSONClient(host, &http.Client{})
	return nil
}

func createRoom(c *cli.Context) error {
	ctx := contextWithAccessToken(c, &auth.VideoGrant{RoomCreate: true})
	room, err := roomClient.CreateRoom(ctx, &livekit.CreateRoomRequest{
		Name: c.String("name"),
	})
	if err != nil {
		return err
	}

	PrintJSON(room)
	return nil
}

func listRooms(c *cli.Context) error {
	ctx := contextWithAccessToken(c, &auth.VideoGrant{RoomList: true})
	res, err := roomClient.ListRooms(ctx, &livekit.ListRoomsRequest{})
	if err != nil {
		return err
	}
	for _, rm := range res.Rooms {
		fmt.Printf("%s\t%s\n", rm.Sid, rm.Name)
	}
	return nil
}

func deleteRoom(c *cli.Context) error {
	ctx := contextWithAccessToken(c, &auth.VideoGrant{RoomCreate: true})
	roomId := c.String("room")
	_, err := roomClient.DeleteRoom(ctx, &livekit.DeleteRoomRequest{
		Room: roomId,
	})
	if err != nil {
		return err
	}

	fmt.Println("deleted room", roomId)
	return nil
}

func contextWithAccessToken(c *cli.Context, grant *auth.VideoGrant) context.Context {
	ctx := context.Background()
	token, err := accessToken(c, grant, "")
	if err != nil {
		logger.Errorw("Could not get access token", "err", err)
	}
	if token != "" {
		header := make(http.Header)
		header.Set("Authorization", "Bearer "+token)
		if tctx, err := twirp.WithHTTPRequestHeaders(ctx, header); err == nil {
			logger.Debugw("requesting with token")
			ctx = tctx
		} else {
			logger.Errorw("Error setting Twirp auth header", "err", err)
		}
	}
	return ctx
}
