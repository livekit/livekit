package service

import (
	"context"
	"time"

	"github.com/thoas/go-funk"

	"github.com/livekit/protocol/livekit"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/rtc"
	"github.com/livekit/livekit-server/pkg/utils"
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

func (s *RoomService) DeleteRoom(ctx context.Context, req *livekit.DeleteRoomRequest) (*livekit.DeleteRoomResponse, error) {
	apiKey := GetApiKey(ctx)
	roomKey := utils.RoomKey(livekit.RoomName(req.Room), apiKey)

	AppendLogFields(ctx, "room", req.Room)
	if err := EnsureCreatePermission(ctx); err != nil {
		return nil, twirpAuthError(err)
	}
	err := s.router.WriteRoomRTC(ctx, roomKey, &livekit.RTCNodeMessage{
		Message: &livekit.RTCNodeMessage_DeleteRoom{
			DeleteRoom: req,
		},
	})
	if err != nil {
		return nil, err
	}

	// we should not return until when the room is confirmed deleted
	err = s.confirmExecution(func() error {
		_, _, _, err := s.roomStore.LoadRoom(ctx, roomKey, false)
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

func (s *RoomService) RemoveParticipant(ctx context.Context, req *livekit.RoomParticipantIdentity) (*livekit.RemoveParticipantResponse, error) {
	apiKey := GetApiKey(ctx)
	roomKey := utils.RoomKey(livekit.RoomName(req.Room), apiKey)

	AppendLogFields(ctx, "room", req.Room, "participant", req.Identity)
	err := s.writeParticipantMessage(ctx, livekit.RoomName(req.Room), roomKey, livekit.ParticipantIdentity(req.Identity), &livekit.RTCNodeMessage{
		Message: &livekit.RTCNodeMessage_RemoveParticipant{
			RemoveParticipant: req,
		},
	})
	if err != nil {
		return nil, err
	}

	err = s.confirmExecution(func() error {
		_, err := s.roomStore.LoadParticipant(ctx, roomKey, livekit.ParticipantIdentity(req.Identity))
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
	apiKey := GetApiKey(ctx)
	roomKey := utils.RoomKey(livekit.RoomName(req.Room), apiKey)

	AppendLogFields(ctx, "room", req.Room, "participant", req.Identity, "track", req.TrackSid, "muted", req.Muted)

	err := s.writeParticipantMessage(ctx, livekit.RoomName(req.Room), roomKey, livekit.ParticipantIdentity(req.Identity), &livekit.RTCNodeMessage{
		Message: &livekit.RTCNodeMessage_MuteTrack{
			MuteTrack: req,
		},
	})
	if err != nil {
		return nil, err
	}

	var track *livekit.TrackInfo
	err = s.confirmExecution(func() error {
		p, err := s.roomStore.LoadParticipant(ctx, roomKey, livekit.ParticipantIdentity(req.Identity))
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

func (s *RoomService) writeParticipantMessage(ctx context.Context, roomName livekit.RoomName, roomKey livekit.RoomKey, identity livekit.ParticipantIdentity, msg *livekit.RTCNodeMessage) error {
	if err := EnsureAdminPermission(ctx, roomName); err != nil {
		return twirpAuthError(err)
	}

	return s.router.WriteParticipantRTC(ctx, roomKey, identity, msg)
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
