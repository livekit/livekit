package commands

import (
	"context"
	"fmt"
	"net/http"

	"github.com/urfave/cli/v2"

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
			},
		},
		{
			Name:   "get-room",
			Before: createClient,
			Action: getRoom,
			Flags: []cli.Flag{
				roomFlag,
				roomHostFlag,
			},
		},
		{
			Name:   "delete-room",
			Before: createClient,
			Action: deleteRoom,
			Flags: []cli.Flag{
				roomFlag,
				roomHostFlag,
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
	room, err := roomClient.CreateRoom(context.Background(), &livekit.CreateRoomRequest{})
	if err != nil {
		return err
	}

	PrintJSON(room)
	return nil
}

func getRoom(c *cli.Context) error {
	roomId := c.String("room-id")
	room, err := roomClient.GetRoom(context.Background(), &livekit.GetRoomRequest{
		RoomId: roomId,
	})
	if err != nil {
		return err
	}

	PrintJSON(room)
	return nil
}

func deleteRoom(c *cli.Context) error {
	roomId := c.String("room-id")
	_, err := roomClient.DeleteRoom(context.Background(), &livekit.DeleteRoomRequest{
		RoomId: roomId,
	})
	if err != nil {
		return err
	}

	fmt.Println("deleted room", roomId)
	return nil
}
