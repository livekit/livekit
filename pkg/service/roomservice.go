package service

import (
	"context"

	"github.com/livekit/protocol/livekit"
	"github.com/pkg/errors"
	"github.com/thoas/go-funk"
	"github.com/twitchtv/twirp"

	"github.com/livekit/livekit-server/pkg/routing"
)

// A rooms service that supports a single node
type RoomService struct {
	router        routing.MessageRouter
	roomAllocator RoomAllocator
	roomStore     RORoomStore
}

func NewRoomService(ra RoomAllocator, rs RORoomStore, router routing.MessageRouter) (svc *RoomService, err error) {
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
	err := s.router.WriteRoomRTC(ctx, livekit.RoomName(req.Room), "", &livekit.RTCNodeMessage{
		Message: &livekit.RTCNodeMessage_DeleteRoom{
			DeleteRoom: req,
		},
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
	err = s.writeRoomMessage(ctx, livekit.RoomName(req.Room), livekit.ParticipantIdentity(req.Identity), &livekit.RTCNodeMessage{
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
	if err = EnsureAdminPermission(ctx, livekit.RoomName(req.Room)); err != nil {
		return nil, twirpAuthError(err)
	}

	participant, err := s.roomStore.LoadParticipant(ctx, livekit.RoomName(req.Room), livekit.ParticipantIdentity(req.Identity))
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

	err = s.writeParticipantMessage(ctx, livekit.RoomName(req.Room), livekit.ParticipantIdentity(req.Identity), &livekit.RTCNodeMessage{
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
	err := s.writeRoomMessage(ctx, livekit.RoomName(req.Room), livekit.ParticipantIdentity(req.Identity), &livekit.RTCNodeMessage{
		Message: &livekit.RTCNodeMessage_UpdateParticipant{
			UpdateParticipant: req,
		},
	})
	if err != nil {
		return nil, err
	}

	participant, err := s.roomStore.LoadParticipant(ctx, livekit.RoomName(req.Room), livekit.ParticipantIdentity(req.Identity))
	if err != nil {
		return nil, err
	}

	participant.Metadata = req.Metadata
	return participant, nil
}

func (s *RoomService) UpdateSubscriptions(ctx context.Context, req *livekit.UpdateSubscriptionsRequest) (*livekit.UpdateSubscriptionsResponse, error) {
	err := s.writeRoomMessage(ctx, livekit.RoomName(req.Room), livekit.ParticipantIdentity(req.Identity), &livekit.RTCNodeMessage{
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
	err := s.writeRoomMessage(ctx, livekit.RoomName(req.Room), "", &livekit.RTCNodeMessage{
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

	room.Metadata = req.Metadata

	err = s.writeRoomMessage(ctx, livekit.RoomName(req.Room), "", &livekit.RTCNodeMessage{
		Message: &livekit.RTCNodeMessage_UpdateRoomMetadata{
			UpdateRoomMetadata: req,
		},
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

func (s *RoomService) writeRoomMessage(ctx context.Context, room livekit.RoomName, identity livekit.ParticipantIdentity, msg *livekit.RTCNodeMessage) error {
	if err := EnsureAdminPermission(ctx, room); err != nil {
		return twirpAuthError(err)
	}

	if identity != "" {
		_, err := s.roomStore.LoadParticipant(ctx, room, identity)
		if err != nil {
			return err
		}
	}

	return s.router.WriteRoomRTC(ctx, room, identity, msg)
}
