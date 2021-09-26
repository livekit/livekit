package service

import (
	"context"

	livekit "github.com/livekit/protocol/proto"
	"github.com/pkg/errors"
	"github.com/thoas/go-funk"
	"github.com/twitchtv/twirp"

	"github.com/livekit/livekit-server/pkg/routing"
)

// A rooms service that supports a single node
type RoomService struct {
	router        routing.Router
	selector      routing.NodeSelector
	roomAllocator *RoomAllocator
	roomStore     RoomStore
}

func NewRoomService(ra *RoomAllocator, rs RoomStore, router routing.Router) (svc *RoomService, err error) {
	svc = &RoomService{
		router:        router,
		roomAllocator: ra,
		roomStore:     rs,
	}
	return
}

func (s *RoomService) CreateRoom(ctx context.Context, req *livekit.CreateRoomRequest) (rm *livekit.Room, err error) {
	if err = EnsureCreatePermission(ctx); err != nil {
		return nil, twirpAuthError(err)
	}

	rm, err = s.roomAllocator.CreateRoom(ctx, req)
	if err != nil {
		err = errors.Wrap(err, "could not create room")
	}

	return
}

func (s *RoomService) ListRooms(ctx context.Context, req *livekit.ListRoomsRequest) (res *livekit.ListRoomsResponse, err error) {
	err = EnsureListPermission(ctx)
	if err != nil {
		return nil, twirpAuthError(err)
	}

	rooms, err := s.roomStore.ListRooms(ctx)
	if err != nil {
		// TODO: translate error codes to twirp
		return
	}

	res = &livekit.ListRoomsResponse{
		Rooms: rooms,
	}
	return
}

func (s *RoomService) DeleteRoom(ctx context.Context, req *livekit.DeleteRoomRequest) (*livekit.DeleteRoomResponse, error) {
	if err := EnsureCreatePermission(ctx); err != nil {
		return nil, twirpAuthError(err)
	}
	// if the room is currently active, RTC node needs to disconnect clients
	// here we are using any user's identity, due to how it works with routing
	participants, err := s.roomStore.ListParticipants(ctx, req.Room)
	if err != nil {
		return nil, err
	}

	if len(participants) > 0 {
		err = s.writeMessage(ctx, req.Room, participants[0].Identity, &livekit.RTCNodeMessage{
			Message: &livekit.RTCNodeMessage_DeleteRoom{
				DeleteRoom: req,
			},
		})
		if err != nil {
			return nil, err
		}
	}

	return &livekit.DeleteRoomResponse{}, nil
}

func (s *RoomService) ListParticipants(ctx context.Context, req *livekit.ListParticipantsRequest) (res *livekit.ListParticipantsResponse, err error) {
	if err = EnsureAdminPermission(ctx, req.Room); err != nil {
		return nil, twirpAuthError(err)
	}

	participants, err := s.roomStore.ListParticipants(ctx, req.Room)
	if err != nil {
		return
	}

	res = &livekit.ListParticipantsResponse{
		Participants: participants,
	}
	return
}

func (s *RoomService) GetParticipant(ctx context.Context, req *livekit.RoomParticipantIdentity) (res *livekit.ParticipantInfo, err error) {
	if err = EnsureAdminPermission(ctx, req.Room); err != nil {
		return nil, twirpAuthError(err)
	}

	participant, err := s.roomStore.LoadParticipant(ctx, req.Room, req.Identity)
	if err != nil {
		return
	}

	res = participant
	return
}

func (s *RoomService) RemoveParticipant(ctx context.Context, req *livekit.RoomParticipantIdentity) (res *livekit.RemoveParticipantResponse, err error) {
	err = s.writeMessage(ctx, req.Room, req.Identity, &livekit.RTCNodeMessage{
		Message: &livekit.RTCNodeMessage_RemoveParticipant{
			RemoveParticipant: req,
		},
	})
	if err != nil {
		return
	}

	res = &livekit.RemoveParticipantResponse{}
	return
}

func (s *RoomService) MutePublishedTrack(ctx context.Context, req *livekit.MuteRoomTrackRequest) (res *livekit.MuteRoomTrackResponse, err error) {
	if err = EnsureAdminPermission(ctx, req.Room); err != nil {
		return nil, twirpAuthError(err)
	}

	participant, err := s.roomStore.LoadParticipant(ctx, req.Room, req.Identity)
	if err != nil {
		return nil, err
	}
	// find the track
	track := funk.Find(participant.Tracks, func(t *livekit.TrackInfo) bool {
		return t.Sid == req.TrackSid
	})
	if track == nil {
		return nil, twirp.NotFoundError(ErrTrackNotFound.Error())
	}

	err = s.writeMessage(ctx, req.Room, req.Identity, &livekit.RTCNodeMessage{
		Message: &livekit.RTCNodeMessage_MuteTrack{
			MuteTrack: req,
		},
	})
	if err != nil {
		return
	}

	res = &livekit.MuteRoomTrackResponse{
		Track: track.(*livekit.TrackInfo),
	}
	// mute might not have happened, reflect desired state
	res.Track.Muted = req.Muted
	return
}

func (s *RoomService) UpdateParticipant(ctx context.Context, req *livekit.UpdateParticipantRequest) (*livekit.ParticipantInfo, error) {
	err := s.writeMessage(ctx, req.Room, req.Identity, &livekit.RTCNodeMessage{
		Message: &livekit.RTCNodeMessage_UpdateParticipant{
			UpdateParticipant: req,
		},
	})
	if err != nil {
		return nil, err
	}

	participant, err := s.roomStore.LoadParticipant(ctx, req.Room, req.Identity)
	if err != nil {
		return nil, err
	}

	participant.Metadata = req.Metadata
	return participant, nil
}

func (s *RoomService) UpdateSubscriptions(ctx context.Context, req *livekit.UpdateSubscriptionsRequest) (*livekit.UpdateSubscriptionsResponse, error) {
	err := s.writeMessage(ctx, req.Room, req.Identity, &livekit.RTCNodeMessage{
		Message: &livekit.RTCNodeMessage_UpdateSubscriptions{
			UpdateSubscriptions: req,
		},
	})
	if err != nil {
		return nil, err
	}

	return &livekit.UpdateSubscriptionsResponse{}, nil
}

func (s *RoomService) SendData(ctx context.Context, req *livekit.SendDataRequest) (*livekit.SendDataResponse, error) {
	// here we are using any user's identity, due to how it works with routing
	participants, err := s.roomStore.ListParticipants(ctx, req.Room)
	if err != nil {
		return nil, err
	}

	if len(participants) > 0 {
		err := s.writeMessage(ctx, req.Room, participants[0].Identity, &livekit.RTCNodeMessage{
			Message: &livekit.RTCNodeMessage_SendData{
				SendData: req,
			},
		})
		if err != nil {
			return nil, err
		}
	}

	return &livekit.SendDataResponse{}, nil
}

func (s *RoomService) UpdateRoomMetadata(ctx context.Context, req *livekit.UpdateRoomMetadataRequest) (*livekit.Room, error) {
	if err := EnsureAdminPermission(ctx, req.Room); err != nil {
		return nil, twirpAuthError(err)
	}

	room, err := s.roomStore.LoadRoom(ctx, req.Room)
	if err != nil {
		return nil, err
	}

	room.Metadata = req.Metadata

	participants, err := s.roomStore.ListParticipants(ctx, req.Room)
	if err != nil {
		return nil, err
	}

	if len(participants) > 0 {
		err := s.writeMessage(ctx, req.Room, participants[0].Identity, &livekit.RTCNodeMessage{
			Message: &livekit.RTCNodeMessage_UpdateRoomMetadata{
				UpdateRoomMetadata: req,
			},
		})
		if err != nil {
			return nil, err
		}
	}

	return room, nil
}

func (s *RoomService) writeMessage(ctx context.Context, room, identity string, msg *livekit.RTCNodeMessage) error {
	if err := EnsureAdminPermission(ctx, room); err != nil {
		return twirpAuthError(err)
	}

	_, err := s.roomStore.LoadParticipant(ctx, room, identity)
	if err != nil {
		return err
	}

	return s.router.WriteRTCMessage(ctx, room, identity, msg)
}
