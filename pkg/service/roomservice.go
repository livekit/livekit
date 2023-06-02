package service

import (
	"context"
	"strconv"
	"time"

	"github.com/pkg/errors"
	"github.com/thoas/go-funk"
	"github.com/twitchtv/twirp"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/rtc"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/rpc"
)

// A rooms service that supports a single node
type RoomService struct {
	roomConf       config.RoomConfig
	apiConf        config.APIConfig
	router         routing.MessageRouter
	roomAllocator  RoomAllocator
	roomStore      ServiceStore
	egressLauncher rtc.EgressLauncher
}

func NewRoomService(
	roomConf config.RoomConfig,
	apiConf config.APIConfig,
	router routing.MessageRouter,
	roomAllocator RoomAllocator,
	serviceStore ServiceStore,
	egressLauncher rtc.EgressLauncher,
) (svc *RoomService, err error) {
	svc = &RoomService{
		roomConf:       roomConf,
		apiConf:        apiConf,
		router:         router,
		roomAllocator:  roomAllocator,
		roomStore:      serviceStore,
		egressLauncher: egressLauncher,
	}
	return
}

func (s *RoomService) CreateRoom(ctx context.Context, req *livekit.CreateRoomRequest) (*livekit.Room, error) {
	AppendLogFields(ctx, "room", req.Name, "request", req)
	if err := EnsureCreatePermission(ctx); err != nil {
		return nil, twirpAuthError(err)
	} else if req.Egress != nil && s.egressLauncher == nil {
		return nil, ErrEgressNotConnected
	}

	rm, err := s.roomAllocator.CreateRoom(ctx, req)
	if err != nil {
		err = errors.Wrap(err, "could not create room")
		return nil, err
	}

	// actually start the room on an RTC node, to ensure metadata & empty timeout functionality
	_, sink, source, err := s.router.StartParticipantSignal(ctx,
		livekit.RoomName(req.Name),
		routing.ParticipantInit{},
	)
	if err != nil {
		return nil, err
	}
	sink.Close()
	source.Close()

	// ensure it's created correctly
	err = s.confirmExecution(func() error {
		_, _, err := s.roomStore.LoadRoom(ctx, livekit.RoomName(req.Name), false)
		if err != nil {
			return ErrOperationFailed
		} else {
			return nil
		}
	})
	if err != nil {
		return nil, err
	}

	if req.Egress != nil && req.Egress.Room != nil {
		egress := &rpc.StartEgressRequest{
			Request: &rpc.StartEgressRequest_RoomComposite{
				RoomComposite: req.Egress.Room,
			},
			RoomId: rm.Sid,
		}
		_, err = s.egressLauncher.StartEgress(ctx, egress)
	}

	return rm, err
}

func (s *RoomService) ListRooms(ctx context.Context, req *livekit.ListRoomsRequest) (*livekit.ListRoomsResponse, error) {
	AppendLogFields(ctx, "room", req.Names)
	err := EnsureListPermission(ctx)
	if err != nil {
		return nil, twirpAuthError(err)
	}

	var names []livekit.RoomName
	if len(req.Names) > 0 {
		names = livekit.StringsAsRoomNames(req.Names)
	}
	rooms, err := s.roomStore.ListRooms(ctx, names)
	if err != nil {
		// TODO: translate error codes to Twirp
		return nil, err
	}

	res := &livekit.ListRoomsResponse{
		Rooms: rooms,
	}
	return res, nil
}

func (s *RoomService) DeleteRoom(ctx context.Context, req *livekit.DeleteRoomRequest) (*livekit.DeleteRoomResponse, error) {
	AppendLogFields(ctx, "room", req.Room)
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
	err = s.confirmExecution(func() error {
		_, _, err := s.roomStore.LoadRoom(ctx, livekit.RoomName(req.Room), false)
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

func (s *RoomService) ListParticipants(ctx context.Context, req *livekit.ListParticipantsRequest) (*livekit.ListParticipantsResponse, error) {
	AppendLogFields(ctx, "room", req.Room)
	if err := EnsureAdminPermission(ctx, livekit.RoomName(req.Room)); err != nil {
		return nil, twirpAuthError(err)
	}

	participants, err := s.roomStore.ListParticipants(ctx, livekit.RoomName(req.Room))
	if err != nil {
		return nil, err
	}

	res := &livekit.ListParticipantsResponse{
		Participants: participants,
	}
	return res, nil
}

func (s *RoomService) GetParticipant(ctx context.Context, req *livekit.RoomParticipantIdentity) (*livekit.ParticipantInfo, error) {
	AppendLogFields(ctx, "room", req.Room, "participant", req.Identity)
	if err := EnsureAdminPermission(ctx, livekit.RoomName(req.Room)); err != nil {
		return nil, twirpAuthError(err)
	}

	participant, err := s.roomStore.LoadParticipant(ctx, livekit.RoomName(req.Room), livekit.ParticipantIdentity(req.Identity))
	if err != nil {
		return nil, err
	}

	return participant, nil
}

func (s *RoomService) RemoveParticipant(ctx context.Context, req *livekit.RoomParticipantIdentity) (*livekit.RemoveParticipantResponse, error) {
	AppendLogFields(ctx, "room", req.Room, "participant", req.Identity)
	err := s.writeParticipantMessage(ctx, livekit.RoomName(req.Room), livekit.ParticipantIdentity(req.Identity), &livekit.RTCNodeMessage{
		Message: &livekit.RTCNodeMessage_RemoveParticipant{
			RemoveParticipant: req,
		},
	})
	if err != nil {
		return nil, err
	}

	err = s.confirmExecution(func() error {
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

	return &livekit.RemoveParticipantResponse{}, nil
}

func (s *RoomService) MutePublishedTrack(ctx context.Context, req *livekit.MuteRoomTrackRequest) (*livekit.MuteRoomTrackResponse, error) {
	AppendLogFields(ctx, "room", req.Room, "participant", req.Identity, "trackID", req.TrackSid, "muted", req.Muted)
	if err := EnsureAdminPermission(ctx, livekit.RoomName(req.Room)); err != nil {
		return nil, twirpAuthError(err)
	}

	err := s.writeParticipantMessage(ctx, livekit.RoomName(req.Room), livekit.ParticipantIdentity(req.Identity), &livekit.RTCNodeMessage{
		Message: &livekit.RTCNodeMessage_MuteTrack{
			MuteTrack: req,
		},
	})
	if err != nil {
		return nil, err
	}

	var track *livekit.TrackInfo
	err = s.confirmExecution(func() error {
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

	res := &livekit.MuteRoomTrackResponse{
		Track: track,
	}
	return res, nil
}

func (s *RoomService) UpdateParticipant(ctx context.Context, req *livekit.UpdateParticipantRequest) (*livekit.ParticipantInfo, error) {
	AppendLogFields(ctx, "room", req.Room, "participant", req.Identity)
	maxMetadataSize := int(s.roomConf.MaxMetadataSize)
	if maxMetadataSize > 0 && len(req.Metadata) > maxMetadataSize {
		return nil, twirp.InvalidArgumentError(ErrMetadataExceedsLimits.Error(), strconv.Itoa(maxMetadataSize))
	}

	err := s.writeParticipantMessage(ctx, livekit.RoomName(req.Room), livekit.ParticipantIdentity(req.Identity), &livekit.RTCNodeMessage{
		Message: &livekit.RTCNodeMessage_UpdateParticipant{
			UpdateParticipant: req,
		},
	})
	if err != nil {
		return nil, err
	}

	var participant *livekit.ParticipantInfo
	err = s.confirmExecution(func() error {
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
	trackSIDs := append(make([]string, 0), req.TrackSids...)
	for _, pt := range req.ParticipantTracks {
		trackSIDs = append(trackSIDs, pt.TrackSids...)
	}
	AppendLogFields(ctx, "room", req.Room, "participant", req.Identity, "trackID", trackSIDs)
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
	AppendLogFields(ctx, "room", roomName, "size", len(req.Data))
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
	AppendLogFields(ctx, "room", req.Room, "size", len(req.Metadata))
	maxMetadataSize := int(s.roomConf.MaxMetadataSize)
	if maxMetadataSize > 0 && len(req.Metadata) > maxMetadataSize {
		return nil, twirp.InvalidArgumentError(ErrMetadataExceedsLimits.Error(), strconv.Itoa(maxMetadataSize))
	}

	if err := EnsureAdminPermission(ctx, livekit.RoomName(req.Room)); err != nil {
		return nil, twirpAuthError(err)
	}

	room, _, err := s.roomStore.LoadRoom(ctx, livekit.RoomName(req.Room), false)
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

	err = s.confirmExecution(func() error {
		room, _, err = s.roomStore.LoadRoom(ctx, livekit.RoomName(req.Room), false)
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

	return s.router.WriteParticipantRTC(ctx, room, identity, msg)
}

func (s *RoomService) confirmExecution(f func() error) error {
	expired := time.After(s.apiConf.ExecutionTimeout)
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
			time.Sleep(s.apiConf.CheckInterval)
		}
	}
}
