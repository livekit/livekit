package service

import (
	"context"
	"encoding/json"
	"errors"

	"google.golang.org/protobuf/proto"

	"github.com/livekit/protocol/egress"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/telemetry"
)

type EgressService struct {
	rpcClient   egress.RPCClient
	store       ServiceStore
	es          EgressStore
	roomService livekit.RoomService
	telemetry   telemetry.TelemetryService
	shutdown    chan struct{}
}

func NewEgressService(
	rpcClient egress.RPCClient,
	store ServiceStore,
	es EgressStore,
	rs livekit.RoomService,
	ts telemetry.TelemetryService,
) *EgressService {

	return &EgressService{
		rpcClient:   rpcClient,
		store:       store,
		es:          es,
		roomService: rs,
		telemetry:   ts,
		shutdown:    make(chan struct{}),
	}
}

func (s *EgressService) Start() {
	if s.rpcClient != nil {
		go s.updateWorker()
	}
}

func (s *EgressService) Stop() {
	close(s.shutdown)
}

func (s *EgressService) StartRoomCompositeEgress(ctx context.Context, req *livekit.RoomCompositeEgressRequest) (*livekit.EgressInfo, error) {
	return s.StartEgress(ctx, livekit.RoomName(req.RoomName), &livekit.StartEgressRequest{
		Request: &livekit.StartEgressRequest_RoomComposite{
			RoomComposite: req,
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
	if s.rpcClient == nil {
		return nil, ErrEgressNotConnected
	}

	room, err := s.store.LoadRoom(ctx, roomName)
	if err != nil {
		return nil, err
	}
	req.RoomId = room.Sid

	info, err := s.rpcClient.SendRequest(ctx, req)
	if err != nil {
		return nil, err
	}

	s.telemetry.EgressStarted(ctx, info)
	go func() {
		if err := s.es.StoreEgress(ctx, info); err != nil {
			logger.Errorw("could not write egress info", err)
		}
	}()

	return info, nil
}

type LayoutMetadata struct {
	Layout string `json:"layout"`
}

func (s *EgressService) UpdateLayout(ctx context.Context, req *livekit.UpdateLayoutRequest) (*livekit.EgressInfo, error) {
	if err := EnsureRecordPermission(ctx); err != nil {
		return nil, twirpAuthError(err)
	}
	if s.rpcClient == nil {
		return nil, ErrEgressNotConnected
	}

	info, err := s.es.LoadEgress(ctx, req.EgressId)
	if err != nil {
		return nil, err
	}

	var roomName string
	switch r := info.Request.(type) {
	case *livekit.EgressInfo_RoomComposite:
		roomName = r.RoomComposite.RoomName
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

	grants := GetGrants(ctx)
	grants.Video.Room = roomName
	grants.Video.RoomAdmin = true

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
	if s.rpcClient == nil {
		return nil, ErrEgressNotConnected
	}

	info, err := s.rpcClient.SendRequest(ctx, &livekit.EgressRequest{
		EgressId: req.EgressId,
		Request: &livekit.EgressRequest_UpdateStream{
			UpdateStream: req,
		},
	})
	if err != nil {
		return nil, err
	}

	go func() {
		if err := s.es.UpdateEgress(ctx, info); err != nil {
			logger.Errorw("could not write egress info", err)
		}
	}()

	return info, nil
}

func (s *EgressService) ListEgress(ctx context.Context, req *livekit.ListEgressRequest) (*livekit.ListEgressResponse, error) {
	if err := EnsureRecordPermission(ctx); err != nil {
		return nil, twirpAuthError(err)
	}
	if s.rpcClient == nil {
		return nil, ErrEgressNotConnected
	}

	var roomID livekit.RoomID
	if req.RoomName != "" {
		room, err := s.store.LoadRoom(ctx, livekit.RoomName(req.RoomName))
		if err != nil {
			if err == ErrRoomNotFound {
				return &livekit.ListEgressResponse{}, nil
			}
			return nil, err
		}
		roomID = livekit.RoomID(room.Sid)
	}

	infos, err := s.es.ListEgress(ctx, roomID)
	if err != nil {
		return nil, err
	}

	return &livekit.ListEgressResponse{Items: infos}, nil
}

func (s *EgressService) StopEgress(ctx context.Context, req *livekit.StopEgressRequest) (*livekit.EgressInfo, error) {
	if err := EnsureRecordPermission(ctx); err != nil {
		return nil, twirpAuthError(err)
	}
	if s.rpcClient == nil {
		return nil, ErrEgressNotConnected
	}

	info, err := s.rpcClient.SendRequest(ctx, &livekit.EgressRequest{
		EgressId: req.EgressId,
		Request: &livekit.EgressRequest_Stop{
			Stop: req,
		},
	})
	if err != nil {
		return nil, err
	}

	go func() {
		if err := s.es.UpdateEgress(ctx, info); err != nil {
			logger.Errorw("could not write egress info", err)
		}
	}()

	return info, nil
}

func (s *EgressService) updateWorker() {
	sub, err := s.rpcClient.GetUpdateChannel(context.Background())
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

			switch res.Status {
			case livekit.EgressStatus_EGRESS_ACTIVE,
				livekit.EgressStatus_EGRESS_ENDING:

				// save updated info to store
				err = s.es.UpdateEgress(context.Background(), res)
				if err != nil {
					logger.Errorw("could not update egress", err)
				}

			case livekit.EgressStatus_EGRESS_COMPLETE,
				livekit.EgressStatus_EGRESS_FAILED,
				livekit.EgressStatus_EGRESS_ABORTED:

				// delete from store
				err = s.es.DeleteEgress(context.Background(), res)
				if err != nil {
					logger.Errorw("could not delete egress from store", err)
				}

				// log results
				if res.Error != "" {
					logger.Errorw("egress failed", errors.New(res.Error), "egressID", res.EgressId)
				} else {
					logger.Infow("egress ended", "egressID", res.EgressId)
				}

				s.telemetry.EgressEnded(context.Background(), res)
			}

		case <-s.shutdown:
			_ = sub.Close()
			return
		}
	}
}
