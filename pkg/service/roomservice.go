package service

import (
	"context"

	"github.com/pkg/errors"
	"github.com/thoas/go-funk"
	"github.com/twitchtv/twirp"

	livekit "github.com/livekit/livekit-server/proto"
)

// A rooms service that supports a single node
type RoomService struct {
	roomManager *RoomManager
}

func NewRoomService(roomManager *RoomManager) (svc *RoomService, err error) {
	svc = &RoomService{
		roomManager: roomManager,
	}

	return
}

func (s *RoomService) CreateRoom(ctx context.Context, req *livekit.CreateRoomRequest) (rm *livekit.Room, err error) {
	if err = EnsureCreatePermission(ctx); err != nil {
		return nil, twirpAuthError(err)
	}

	rm, err = s.roomManager.CreateRoom(req)
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

	rooms, err := s.roomManager.roomStore.ListRooms()
	if err != nil {
		// TODO: translate error codes to twirp
		return
	}

	res = &livekit.ListRoomsResponse{
		Rooms: rooms,
	}
	return
}

func (s *RoomService) DeleteRoom(ctx context.Context, req *livekit.DeleteRoomRequest) (res *livekit.DeleteRoomResponse, err error) {
	if err = EnsureCreatePermission(ctx); err != nil {
		return nil, twirpAuthError(err)
	}
	err = s.roomManager.DeleteRoom(req.Room)
	if err != nil {
		err = twirp.WrapError(twirp.InternalError("could not delete room"), err)
		return
	}
	res = &livekit.DeleteRoomResponse{}
	return
}

func (s *RoomService) ListParticipants(ctx context.Context, req *livekit.ListParticipantsRequest) (res *livekit.ListParticipantsResponse, err error) {
	if err = EnsureAdminPermission(ctx, req.Room); err != nil {
		return nil, twirpAuthError(err)
	}

	participants, err := s.roomManager.roomStore.ListParticipants(req.Room)
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

	participant, err := s.roomManager.roomStore.GetParticipant(req.Room, req.Identity)
	if err != nil {
		return
	}

	res = participant
	return
}

func (s *RoomService) RemoveParticipant(ctx context.Context, req *livekit.RoomParticipantIdentity) (res *livekit.RemoveParticipantResponse, err error) {
	if err = EnsureAdminPermission(ctx, req.Room); err != nil {
		return nil, twirpAuthError(err)
	}

	participant, err := s.roomManager.roomStore.GetParticipant(req.Room, req.Identity)
	if err != nil {
		return
	}

	rtcSink, err := s.roomManager.router.CreateRTCSink(req.Room, participant.Identity)
	if err != nil {
		return
	}
	defer rtcSink.Close()
	err = rtcSink.WriteMessage(&livekit.RTCNodeMessage{
		Message: &livekit.RTCNodeMessage_RemoveParticipant{
			RemoveParticipant: req,
		},
	})

	res = &livekit.RemoveParticipantResponse{}
	return
}

func (s *RoomService) MutePublishedTrack(ctx context.Context, req *livekit.MuteRoomTrackRequest) (res *livekit.MuteRoomTrackResponse, err error) {
	if err = EnsureAdminPermission(ctx, req.Room); err != nil {
		return nil, twirpAuthError(err)
	}

	participant, err := s.roomManager.roomStore.GetParticipant(req.Room, req.Identity)
	if err != nil {
		return
	}

	rtcSink, err := s.roomManager.router.CreateRTCSink(req.Room, participant.Identity)
	if err != nil {
		return
	}
	defer rtcSink.Close()
	err = rtcSink.WriteMessage(&livekit.RTCNodeMessage{
		Message: &livekit.RTCNodeMessage_MuteTrack{
			MuteTrack: req,
		},
	})

	if err != nil {
		return
	}

	// find the track
	track := funk.Find(participant.Tracks, func(t *livekit.TrackInfo) bool {
		return t.Sid == req.TrackSid
	})
	if track == nil {
		return nil, twirp.NotFoundError(ErrTrackNotFound.Error())
	}
	res = &livekit.MuteRoomTrackResponse{
		Track: track.(*livekit.TrackInfo),
	}
	return
}
