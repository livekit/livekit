package service

import (
	"context"
	"time"

	"github.com/pkg/errors"
	"github.com/thoas/go-funk"
	"github.com/twitchtv/twirp"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/protocol/livekit"

	"github.com/livekit/livekit-server/pkg/routing"
)

const (
	executionTimeout = 2 * time.Second
	checkInterval    = 50 * time.Millisecond
)

// A rooms service that supports a single node
type RoomService struct {
	router        routing.MessageRouter
	roomAllocator RoomAllocator
	roomStore     ServiceStore
}

func NewRoomService(ra RoomAllocator, rs ServiceStore, router routing.MessageRouter) (svc *RoomService, err error) {
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

	var names []livekit.RoomName
	if len(req.Names) > 0 {
		names = livekit.StringsAsRoomNames(req.Names)
	}
	rooms, err := s.roomStore.ListRooms(ctx, names)
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
	err := s.router.WriteRoomRTC(ctx, livekit.RoomName(req.Room), &livekit.RTCNodeMessage{
		Message: &livekit.RTCNodeMessage_DeleteRoom{
			DeleteRoom: req,
		},
	})
	if err != nil {
		return nil, err
	}

	// we should not return until when the room is confirmed deleted
	err = confirmExecution(func() error {
		_, err := s.roomStore.LoadRoom(ctx, livekit.RoomName(req.Room))
		if err == nil {
			return ErrOperationFailed
		} else if err != ErrRoomNotFound {
			return err
		} else {
			return nil
		}
	})
	if err != nil {
		return nil, err
	}

	return &livekit.DeleteRoomResponse{}, nil
}

func (s *RoomService) ListParticipants(ctx context.Context, req *livekit.ListParticipantsRequest) (res *livekit.ListParticipantsResponse, err error) {
	if err = EnsureAdminPermission(ctx, livekit.RoomName(req.Room)); err != nil {
		return nil, twirpAuthError(err)
	}

	participants, err := s.roomStore.ListParticipants(ctx, livekit.RoomName(req.Room))
	if err != nil {
		return
	}

	res = &livekit.ListParticipantsResponse{
		Participants: participants,
	}
	return
}

func (s *RoomService) GetParticipant(ctx context.Context, req *livekit.RoomParticipantIdentity) (res *livekit.ParticipantInfo, err error) {
	if err = EnsureAdminPermission(ctx, livekit.RoomName(req.Room)); err != nil {
		return nil, twirpAuthError(err)
	}

	participant, err := s.roomStore.LoadParticipant(ctx, livekit.RoomName(req.Room), livekit.ParticipantIdentity(req.Identity))
	if err != nil {
		return
	}

	res = participant
	return
}

func (s *RoomService) RemoveParticipant(ctx context.Context, req *livekit.RoomParticipantIdentity) (res *livekit.RemoveParticipantResponse, err error) {
	err = s.writeParticipantMessage(ctx, livekit.RoomName(req.Room), livekit.ParticipantIdentity(req.Identity), &livekit.RTCNodeMessage{
		Message: &livekit.RTCNodeMessage_RemoveParticipant{
			RemoveParticipant: req,
		},
	})
	if err != nil {
		return
	}

	err = confirmExecution(func() error {
		_, err := s.roomStore.LoadParticipant(ctx, livekit.RoomName(req.Room), livekit.ParticipantIdentity(req.Identity))
		if err == ErrParticipantNotFound {
			return nil
		} else if err != nil {
			return err
		} else {
			return ErrOperationFailed
		}
	})
	if err != nil {
		return nil, err
	}

	res = &livekit.RemoveParticipantResponse{}
	return
}

func (s *RoomService) MutePublishedTrack(ctx context.Context, req *livekit.MuteRoomTrackRequest) (res *livekit.MuteRoomTrackResponse, err error) {
	if err = EnsureAdminPermission(ctx, livekit.RoomName(req.Room)); err != nil {
		return nil, twirpAuthError(err)
	}

	participant, err := s.roomStore.LoadParticipant(ctx, livekit.RoomName(req.Room), livekit.ParticipantIdentity(req.Identity))
	if err != nil {
		return nil, err
	}
	// find the track
	t := funk.Find(participant.Tracks, func(t *livekit.TrackInfo) bool {
		return t.Sid == req.TrackSid
	})
	if t == nil {
		return nil, twirp.NotFoundError(ErrTrackNotFound.Error())
	}

	err = s.writeParticipantMessage(ctx, livekit.RoomName(req.Room), livekit.ParticipantIdentity(req.Identity), &livekit.RTCNodeMessage{
		Message: &livekit.RTCNodeMessage_MuteTrack{
			MuteTrack: req,
		},
	})
	if err != nil {
		return
	}

	var track *livekit.TrackInfo
	err = confirmExecution(func() error {
		p, err := s.roomStore.LoadParticipant(ctx, livekit.RoomName(req.Room), livekit.ParticipantIdentity(req.Identity))
		if err != nil {
			return err
		}
		// ensure track is muted
		t := funk.Find(p.Tracks, func(t *livekit.TrackInfo) bool {
			return t.Sid == req.TrackSid
		})
		var ok bool
		track, ok = t.(*livekit.TrackInfo)
		if !ok {
			return ErrTrackNotFound
		}
		if track.Muted != req.Muted {
			return ErrOperationFailed
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	res = &livekit.MuteRoomTrackResponse{
		Track: track,
	}
	return
}

func (s *RoomService) UpdateParticipant(ctx context.Context, req *livekit.UpdateParticipantRequest) (*livekit.ParticipantInfo, error) {
	err := s.writeParticipantMessage(ctx, livekit.RoomName(req.Room), livekit.ParticipantIdentity(req.Identity), &livekit.RTCNodeMessage{
		Message: &livekit.RTCNodeMessage_UpdateParticipant{
			UpdateParticipant: req,
		},
	})
	if err != nil {
		return nil, err
	}

	var participant *livekit.ParticipantInfo
	err = confirmExecution(func() error {
		participant, err = s.roomStore.LoadParticipant(ctx, livekit.RoomName(req.Room), livekit.ParticipantIdentity(req.Identity))
		if err != nil {
			return err
		}
		if req.Metadata != "" && participant.Metadata != req.Metadata {
			return ErrOperationFailed
		}
		if req.Permission != nil && !proto.Equal(req.Permission, participant.Permission) {
			return ErrOperationFailed
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return participant, nil
}

func (s *RoomService) UpdateSubscriptions(ctx context.Context, req *livekit.UpdateSubscriptionsRequest) (*livekit.UpdateSubscriptionsResponse, error) {
	err := s.writeParticipantMessage(ctx, livekit.RoomName(req.Room), livekit.ParticipantIdentity(req.Identity), &livekit.RTCNodeMessage{
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
	roomName := livekit.RoomName(req.Room)
	if err := EnsureAdminPermission(ctx, roomName); err != nil {
		return nil, twirpAuthError(err)
	}

	err := s.router.WriteRoomRTC(ctx, roomName, &livekit.RTCNodeMessage{
		Message: &livekit.RTCNodeMessage_SendData{
			SendData: req,
		},
	})
	if err != nil {
		return nil, err
	}

	return &livekit.SendDataResponse{}, nil
}

func (s *RoomService) UpdateRoomMetadata(ctx context.Context, req *livekit.UpdateRoomMetadataRequest) (*livekit.Room, error) {
	if err := EnsureAdminPermission(ctx, livekit.RoomName(req.Room)); err != nil {
		return nil, twirpAuthError(err)
	}

	room, err := s.roomStore.LoadRoom(ctx, livekit.RoomName(req.Room))
	if err != nil {
		return nil, err
	}

	// no one has joined the room, would not have been created on an RTC node.
	// in this case, we'd want to run create again
	_, err = s.roomAllocator.CreateRoom(ctx, &livekit.CreateRoomRequest{
		Name:     req.Room,
		Metadata: req.Metadata,
	})
	if err != nil {
		return nil, err
	}

	err = s.router.WriteRoomRTC(ctx, livekit.RoomName(req.Room), &livekit.RTCNodeMessage{
		Message: &livekit.RTCNodeMessage_UpdateRoomMetadata{
			UpdateRoomMetadata: req,
		},
	})
	if err != nil {
		return nil, err
	}

	err = confirmExecution(func() error {
		room, err = s.roomStore.LoadRoom(ctx, livekit.RoomName(req.Room))
		if err != nil {
			return err
		}
		if room.Metadata != req.Metadata {
			return ErrOperationFailed
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return room, nil
}

func (s *RoomService) writeParticipantMessage(ctx context.Context, room livekit.RoomName, identity livekit.ParticipantIdentity, msg *livekit.RTCNodeMessage) error {
	if err := EnsureAdminPermission(ctx, room); err != nil {
		return twirpAuthError(err)
	}

	_, err := s.roomStore.LoadParticipant(ctx, room, identity)
	if err != nil {
		return err
	}

	return s.router.WriteParticipantRTC(ctx, room, identity, msg)
}

func confirmExecution(f func() error) error {
	expired := time.After(executionTimeout)
	var err error
	for {
		select {
		case <-expired:
			return err
		default:
			err = f()
			if err == nil {
				return nil
			}
			time.Sleep(checkInterval)
		}
	}
}
