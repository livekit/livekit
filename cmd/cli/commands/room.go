package commands

import (
	"context"
	"fmt"
	"net/http"

	"github.com/twitchtv/twirp"
	"github.com/urfave/cli/v2"

	"github.com/livekit/livekit-server/cmd/cli/client"
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
		{
			Name:   "list-participants",
			Before: createClient,
			Action: listParticipants,
			Flags: []cli.Flag{
				roomFlag,
				roomHostFlag,
				apiKeyFlag,
				secretFlag,
			},
		},
		{
			Name:   "get-participant",
			Before: createClient,
			Action: getParticipant,
			Flags: []cli.Flag{
				roomFlag,
				identityFlag,
				roomHostFlag,
				apiKeyFlag,
				secretFlag,
			},
		},
		{
			Name:   "remove-participant",
			Before: createClient,
			Action: removeParticipant,
			Flags: []cli.Flag{
				roomFlag,
				identityFlag,
				roomHostFlag,
				apiKeyFlag,
				secretFlag,
			},
		},
		{
			Name:   "mute-track",
			Before: createClient,
			Action: muteTrack,
			Flags: []cli.Flag{
				roomFlag,
				identityFlag,
				&cli.StringFlag{
					Name:     "track",
					Usage:    "track sid to mute",
					Required: true,
				},
				&cli.BoolFlag{
					Name:     "muted",
					Usage:    "set to true to mute, false to unmute",
					Required: true,
				},
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

func listParticipants(c *cli.Context) error {
	roomName := c.String("room")
	ctx := contextWithAccessToken(c, &auth.VideoGrant{RoomAdmin: true, Room: roomName})
	res, err := roomClient.ListParticipants(ctx, &livekit.ListParticipantsRequest{
		Room: roomName,
	})
	if err != nil {
		return err
	}

	for _, p := range res.Participants {
		fmt.Printf("%s (%s)\t tracks: %d\n", p.Identity, p.State.String(), len(p.Tracks))
	}
	return nil
}

func getParticipant(c *cli.Context) error {
	roomName := c.String("room")
	identity := c.String("identity")
	ctx := contextWithAccessToken(c, &auth.VideoGrant{RoomAdmin: true, Room: roomName})
	res, err := roomClient.GetParticipant(ctx, &livekit.RoomParticipantIdentity{
		Room:     roomName,
		Identity: identity,
	})
	if err != nil {
		return err
	}

	PrintJSON(res)

	return nil
}

func removeParticipant(c *cli.Context) error {
	roomName := c.String("room")
	identity := c.String("identity")
	ctx := contextWithAccessToken(c, &auth.VideoGrant{RoomAdmin: true, Room: roomName})
	_, err := roomClient.RemoveParticipant(ctx, &livekit.RoomParticipantIdentity{
		Room:     roomName,
		Identity: identity,
	})
	if err != nil {
		return err
	}

	fmt.Println("successfully removed participant", identity)

	return nil
}

func muteTrack(c *cli.Context) error {
	roomName := c.String("room")
	identity := c.String("identity")
	trackSid := c.String("track")
	ctx := contextWithAccessToken(c, &auth.VideoGrant{RoomAdmin: true, Room: roomName})
	_, err := roomClient.MutePublishedTrack(ctx, &livekit.MuteRoomTrackRequest{
		Room:     roomName,
		Identity: identity,
		TrackSid: trackSid,
		Muted:    c.Bool("muted"),
	})
	if err != nil {
		return err
	}

	fmt.Println("muted track", trackSid)
	return nil
}

func contextWithAccessToken(c *cli.Context, grant *auth.VideoGrant) context.Context {
	ctx := context.Background()
	token, err := accessToken(c, grant, "").ToJWT()
	if err != nil {
		logger.Errorw("Could not get access token", "err", err)
	}
	if token != "" {
		header := make(http.Header)
		client.SetAuthorizationToken(header, token)
		if tctx, err := twirp.WithHTTPRequestHeaders(ctx, header); err == nil {
			logger.Debugw("requesting with token")
			ctx = tctx
		} else {
			logger.Errorw("Error setting Twirp auth header", "err", err)
		}
	}
	return ctx
}
