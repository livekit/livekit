package service

import (
	"context"

	"github.com/pkg/errors"
	"github.com/thoas/go-funk"
	"github.com/twitchtv/twirp"

	"github.com/livekit/livekit-server/pkg/routing"
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

func (s *RoomService) DeleteRoom(ctx context.Context, req *livekit.DeleteRoomRequest) (*livekit.DeleteRoomResponse, error) {
	if err := EnsureCreatePermission(ctx); err != nil {
		return nil, twirpAuthError(err)
	}
	// if the room is currently active, RTC node needs to disconnect clients
	// here we are using any user's identity, due to how it works with routing
	participants, err := s.roomManager.roomStore.ListParticipants(req.Room, true)
	if err != nil {
		return nil, err
	}

	if len(participants) > 0 {
		rtcSink, err := s.roomManager.router.CreateRTCSink(req.Room, participants[0].Identity)
		if err != nil {
			return nil, err
		}

		defer rtcSink.Close()
		_ = rtcSink.WriteMessage(&livekit.RTCNodeMessage{
			Message: &livekit.RTCNodeMessage_DeleteRoom{
				DeleteRoom: req,
			},
		})
	} else {
		// if a room hasn't started, delete locally
		if err = s.roomManager.DeleteRoom(req.Room); err != nil {
			err = twirp.WrapError(twirp.InternalError("could not delete room"), err)
			return nil, err
		}
	}

	return &livekit.DeleteRoomResponse{}, nil
}

func (s *RoomService) ListParticipants(ctx context.Context, req *livekit.ListParticipantsRequest) (res *livekit.ListParticipantsResponse, err error) {
	if err = EnsureAdminPermission(ctx, req.Room); err != nil {
		return nil, twirpAuthError(err)
	}

	participants, err := s.roomManager.roomStore.ListParticipants(req.Room, false)
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

	rtcSink, err := s.createRTCSink(ctx, req.Room, req.Identity)

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

	rtcSink, err := s.createRTCSink(ctx, req.Room, req.Identity)
	if err != nil {
		return
	}
	defer rtcSink.Close()

	participant, err := s.roomManager.roomStore.GetParticipant(req.Room, req.Identity)
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

	err = rtcSink.WriteMessage(&livekit.RTCNodeMessage{
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
	if err := EnsureAdminPermission(ctx, req.Room); err != nil {
		return nil, twirpAuthError(err)
	}

	rtcSink, err := s.createRTCSink(ctx, req.Room, req.Identity)
	if err != nil {
		return nil, err
	}
	defer rtcSink.Close()

	participant, err := s.roomManager.roomStore.GetParticipant(req.Room, req.Identity)
	if err != nil {
		return nil, err
	}

	err = rtcSink.WriteMessage(&livekit.RTCNodeMessage{
		Message: &livekit.RTCNodeMessage_UpdateParticipant{
			UpdateParticipant: req,
		},
	})

	participant.Metadata = req.Metadata
	return participant, nil
}

func (s *RoomService) UpdateSubscriptions(ctx context.Context, req *livekit.UpdateSubscriptionsRequest) (*livekit.UpdateSubscriptionsResponse, error) {
	if err := EnsureAdminPermission(ctx, req.Room); err != nil {
		return nil, twirpAuthError(err)
	}

	rtcSink, err := s.createRTCSink(ctx, req.Room, req.Identity)
	if err != nil {
		return nil, err
	}
	defer rtcSink.Close()

	err = rtcSink.WriteMessage(&livekit.RTCNodeMessage{
		Message: &livekit.RTCNodeMessage_UpdateSubscriptions{
			UpdateSubscriptions: req,
		},
	})

	return &livekit.UpdateSubscriptionsResponse{}, nil
}

func (s *RoomService) createRTCSink(ctx context.Context, room, identity string) (routing.MessageSink, error) {
	if err := EnsureAdminPermission(ctx, room); err != nil {
		return nil, twirpAuthError(err)
	}

	_, err := s.roomManager.roomStore.GetParticipant(room, identity)
	if err != nil {
		return nil, err
	}

	return s.roomManager.router.CreateRTCSink(room, identity)
}
