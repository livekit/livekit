package service

import (
	"context"

	"github.com/pkg/errors"
	"github.com/thoas/go-funk"
	"github.com/twitchtv/twirp"

	"github.com/livekit/livekit-server/pkg/recording"
	"github.com/livekit/livekit-server/pkg/routing"
	livekit "github.com/livekit/livekit-server/proto"
)

// A rooms service that supports a single node
type RoomService struct {
	roomManager *RoomManager
	recorder    *recording.RoomRecorder
}

func NewRoomService(roomManager *RoomManager, rs *recording.RoomRecorder) (svc *RoomService, err error) {
	svc = &RoomService{
		roomManager: roomManager,
		recorder:    rs,
	}

	return
}

func (s *RoomService) CreateRoom(ctx context.Context, req *livekit.CreateRoomRequest) (rm *livekit.Room, err error) {
	if err = EnsureCreatePermission(ctx); err != nil {
		return nil, twirpAuthError(err)
	}

	var recordingID string
	if req.Recording != nil {
		if s.recorder == nil {
			return nil, errors.New("recording not configured (redis required)")
		}

		recordingID, err = s.recorder.ReserveRecorder(ctx, req.Recording)
		if err != nil {
			err = errors.Wrap(err, "could not reserve recorder")
			return
		}
	}

	rm, err = s.roomManager.CreateRoom(req)
	if err != nil {
		err = errors.Wrap(err, "could not create room")
	}

	if recordingID != "" {
		err = s.recorder.StartRecording(ctx, recordingID)
		if err != nil {
			err = errors.Wrap(err, "could not start recording")
		}
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
	participants, err := s.roomManager.roomStore.ListParticipants(req.Room)
	if err != nil {
		return nil, err
	}

	if len(participants) > 0 {
		err := s.writeMessage(ctx, req.Room, participants[0].Identity, &livekit.RTCNodeMessage{
			Message: &livekit.RTCNodeMessage_DeleteRoom{
				DeleteRoom: req,
			},
		})
		if err != nil {
			return nil, err
		}
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

	err = s.writeMessage(ctx, req.Room, req.Identity, &livekit.RTCNodeMessage{
		ParticipantKey: routing.ParticipantKey(req.Room, req.Identity),
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

	participant, err := s.roomManager.roomStore.GetParticipant(req.Room, req.Identity)
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

func (s *RoomService) StartRecording(ctx context.Context, req *livekit.RecordRoomRequest) (*livekit.RecordingResponse, error) {
	if err := EnsureAdminPermission(ctx, ""); err != nil {
		return nil, twirpAuthError(err)
	}

	if s.recorder == nil {
		return nil, errors.New("recording not configured (redis required)")
	}

	id, err := s.recorder.ReserveRecorder(ctx, req)
	if err != nil {
		return nil, err
	}

	err = s.recorder.StartRecording(ctx, id)
	if err != nil {
		return nil, err
	}

	return &livekit.RecordingResponse{RecordingId: id}, nil
}

func (s *RoomService) EndRecording(ctx context.Context, req *livekit.EndRecordingRequest) (*livekit.RecordingResponse, error) {
	if err := EnsureAdminPermission(ctx, ""); err != nil {
		return nil, twirpAuthError(err)
	}

	if s.recorder == nil {
		return nil, errors.New("recording not configured (redis required)")
	}

	err := s.recorder.EndRecording(ctx, req.RecordingId)
	if err != nil {
		return nil, err
	}
	return &livekit.RecordingResponse{RecordingId: req.RecordingId}, nil
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

func (s *RoomService) writeMessage(ctx context.Context, room, identity string, msg *livekit.RTCNodeMessage) error {
	rtcSink, err := s.createRTCSink(ctx, room, identity)
	if err != nil {
		return err
	}
	defer rtcSink.Close()

	msg.ParticipantKey = routing.ParticipantKey(room, identity)
	return rtcSink.WriteMessage(msg)
}
