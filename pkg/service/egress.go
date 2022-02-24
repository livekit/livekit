package service

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/livekit/protocol/egress"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/recording"
	"github.com/livekit/protocol/utils"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/livekit-server/pkg/telemetry"
)

type EgressService struct {
	bus         utils.MessageBus
	store       EgressStore
	roomService livekit.RoomService
	telemetry   telemetry.TelemetryService
	shutdown    chan struct{}
}

func NewEgressService(bus utils.MessageBus, es EgressStore, rs livekit.RoomService, ts telemetry.TelemetryService) *EgressService {
	return &EgressService{
		bus:         bus,
		store:       es,
		roomService: rs,
		telemetry:   ts,
		shutdown:    make(chan struct{}),
	}
}

func (s *EgressService) Start() {
	if s.bus != nil {
		go s.resultsWorker()
	}
}

func (s *EgressService) Stop() {
	close(s.shutdown)
}

func (s *EgressService) StartWebCompositeEgress(ctx context.Context, req *livekit.WebCompositeEgressRequest) (*livekit.EgressInfo, error) {
	return s.StartEgress(ctx, livekit.RoomName(req.RoomName), &livekit.StartEgressRequest{
		Request: &livekit.StartEgressRequest_WebComposite{
			WebComposite: req,
		},
	})
}

func (s *EgressService) StartTrackCompositeEgress(ctx context.Context, req *livekit.TrackCompositeEgressRequest) (*livekit.EgressInfo, error) {
	return s.StartEgress(ctx, livekit.RoomName(req.RoomName), &livekit.StartEgressRequest{
		Request: &livekit.StartEgressRequest_TrackComposite{
			TrackComposite: req,
		},
	})
}

func (s *EgressService) StartTrackEgress(ctx context.Context, req *livekit.TrackEgressRequest) (*livekit.EgressInfo, error) {
	return s.StartEgress(ctx, livekit.RoomName(req.RoomName), &livekit.StartEgressRequest{
		Request: &livekit.StartEgressRequest_Track{
			Track: req,
		},
	})
}

func (s *EgressService) StartEgress(ctx context.Context, roomName livekit.RoomName, req *livekit.StartEgressRequest) (*livekit.EgressInfo, error) {
	if err := EnsureRecordPermission(ctx); err != nil {
		return nil, twirpAuthError(err)
	}
	if s.bus == nil {
		return nil, errors.New("egress not connected (redis required)")
	}

	room, err := s.store.LoadRoom(ctx, roomName)
	if err != nil {
		return nil, err
	}
	req.RoomId = room.Sid

	info, err := egress.SendRequest(ctx, s.bus, req)
	if err != nil {
		s.telemetry.EgressStarted(ctx, info)
		go func() {
			if err := s.store.StoreEgress(ctx, info); err != nil {
				logger.Errorw("could not write egress info", err)
			}
		}()
	}

	return info, nil
}

type LayoutMetadata struct {
	Layout string `json:"layout"`
}

func (s *EgressService) UpdateLayout(ctx context.Context, req *livekit.UpdateLayoutRequest) (*livekit.EgressInfo, error) {
	if err := EnsureRecordPermission(ctx); err != nil {
		return nil, twirpAuthError(err)
	}
	if s.bus == nil {
		return nil, errors.New("egress not connected (redis required)")
	}

	info, err := s.store.LoadEgress(ctx, req.EgressId)
	if err != nil {
		return nil, err
	}

	var roomName string
	switch r := info.Request.(type) {
	case *livekit.EgressInfo_WebComposite:
		roomName = r.WebComposite.RoomName
	case *livekit.EgressInfo_TrackComposite:
		roomName = r.TrackComposite.RoomName
	case *livekit.EgressInfo_Track:
		roomName = r.Track.RoomName
	default:
		return nil, ErrRoomNotFound
	}

	metadata, err := json.Marshal(&LayoutMetadata{Layout: req.Layout})
	if err != nil {
		return nil, err
	}

	_, err = s.roomService.UpdateParticipant(ctx, &livekit.UpdateParticipantRequest{
		Room:     roomName,
		Identity: info.EgressId,
		Metadata: string(metadata),
	})
	if err != nil {
		return nil, err
	}

	return info, nil
}

func (s *EgressService) UpdateStream(ctx context.Context, req *livekit.UpdateStreamRequest) (*livekit.EgressInfo, error) {
	if err := EnsureRecordPermission(ctx); err != nil {
		return nil, twirpAuthError(err)
	}
	if s.bus == nil {
		return nil, errors.New("egress not connected (redis required)")
	}

	info, err := egress.SendRequest(ctx, s.bus, &livekit.EgressRequest{
		EgressId: req.EgressId,
		Request: &livekit.EgressRequest_UpdateStream{
			UpdateStream: req,
		},
	})
	if err != nil {
		return nil, err
	}

	go func() {
		if err := s.store.UpdateEgress(ctx, info); err != nil {
			logger.Errorw("could not write egress info", err)
		}
	}()

	return info, nil
}

func (s *EgressService) ListEgress(ctx context.Context, req *livekit.ListEgressRequest) (*livekit.ListEgressResponse, error) {
	if err := EnsureRecordPermission(ctx); err != nil {
		return nil, twirpAuthError(err)
	}
	if s.bus == nil {
		return nil, errors.New("egress not connected (redis required)")
	}

	var roomID livekit.RoomID
	if req.RoomName != "" {
		room, err := s.store.LoadRoom(ctx, livekit.RoomName(req.RoomName))
		if err != nil {
			return nil, err
		}
		roomID = livekit.RoomID(room.Sid)
	}

	infos, err := s.store.ListEgress(ctx, roomID)
	if err != nil {
		return nil, err
	}

	return &livekit.ListEgressResponse{Items: infos}, nil
}

func (s *EgressService) StopEgress(ctx context.Context, req *livekit.StopEgressRequest) (*livekit.EgressInfo, error) {
	if err := EnsureRecordPermission(ctx); err != nil {
		return nil, twirpAuthError(err)
	}
	if s.bus == nil {
		return nil, errors.New("recording not configured (redis required)")
	}

	info, err := egress.SendRequest(ctx, s.bus, &livekit.EgressRequest{
		EgressId: req.EgressId,
		Request: &livekit.EgressRequest_Stop{
			Stop: req,
		},
	})
	if err != nil {
		return nil, err
	}

	go func() {
		if err := s.store.UpdateEgress(ctx, info); err != nil {
			logger.Errorw("could not write egress info", err)
		}
	}()

	return info, nil
}

func (s *EgressService) resultsWorker() {
	sub, err := s.bus.SubscribeQueue(context.Background(), recording.ResultChannel)
	if err != nil {
		logger.Errorw("failed to subscribe to results channel", err)
		return
	}

	resChan := sub.Channel()
	for {
		select {
		case msg := <-resChan:
			b := sub.Payload(msg)

			res := &livekit.EgressInfo{}
			if err = proto.Unmarshal(b, res); err != nil {
				logger.Errorw("failed to read results", err)
				continue
			}

			err = s.store.DeleteEgress(context.Background(), res)
			if err != nil {
				logger.Errorw("could not delete egress from store", err)
			}

			// log results
			if res.Error != "" {
				logger.Errorw("egress failed", errors.New(res.Error), "egressID", res.EgressId)
			} else {
				logger.Debugw("egress ended", "egressID", res.EgressId)
			}

			s.telemetry.EgressEnded(context.Background(), res)
		case <-s.shutdown:
			_ = sub.Close()
			return
		}
	}
}
