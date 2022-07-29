package service

import (
	"context"
	"encoding/json"
	"errors"
	"time"

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
	}
}

func (s *EgressService) Start() error {
	if s.shutdown != nil {
		return nil
	}

	s.shutdown = make(chan struct{})
	if s.rpcClient != nil && s.es != nil {
		return s.startWorker()
	}

	return nil
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

	ensureRoomName(info)

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

	ensureRoomName(info)

	metadata, err := json.Marshal(&LayoutMetadata{Layout: req.Layout})
	if err != nil {
		return nil, err
	}

	grants := GetGrants(ctx)
	grants.Video.Room = info.RoomName
	grants.Video.RoomAdmin = true

	_, err = s.roomService.UpdateParticipant(ctx, &livekit.UpdateParticipantRequest{
		Room:     info.RoomName,
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

	ensureRoomName(info)

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

	infos, err := s.es.ListEgress(ctx, livekit.RoomName(req.RoomName))
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

	ensureRoomName(info)

	go func() {
		if err := s.es.UpdateEgress(ctx, info); err != nil {
			logger.Errorw("could not write egress info", err)
		}
	}()

	return info, nil
}

func (s *EgressService) startWorker() error {
	rs := s.es.(*RedisStore)
	if err := rs.Start(); err != nil {
		logger.Errorw("failed to start redis egress worker", err)
		return err
	}

	sub, err := s.rpcClient.GetUpdateChannel(context.Background())
	if err != nil {
		logger.Errorw("failed to subscribe to results channel", err)
		return err
	}

	go func() {
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

				ensureRoomName(res)

				switch res.Status {
				case livekit.EgressStatus_EGRESS_COMPLETE,
					livekit.EgressStatus_EGRESS_FAILED,
					livekit.EgressStatus_EGRESS_ABORTED:

					// make sure endedAt is set so it eventually gets deleted
					if res.EndedAt == 0 {
						res.EndedAt = time.Now().UnixNano()
					}

					err = s.es.UpdateEgress(context.Background(), res)
					if err != nil {
						logger.Errorw("could not update egress", err)
					}

					// log results
					if res.Error != "" {
						logger.Errorw("egress failed", errors.New(res.Error), "egressID", res.EgressId)
					} else {
						logger.Infow("egress ended", "egressID", res.EgressId)
					}

					s.telemetry.EgressEnded(context.Background(), res)

				default:
					err = s.es.UpdateEgress(context.Background(), res)
					if err != nil {
						logger.Errorw("could not update egress", err)
					}
				}

			case <-s.shutdown:
				_ = sub.Close()
				rs.Stop()
				return
			}
		}
	}()

	return nil
}

// Ensure compatibility with Egress <= v1.0.5
func ensureRoomName(info *livekit.EgressInfo) {
	if info.RoomName == "" {
		if info.RoomName == "" {
			switch r := info.Request.(type) {
			case *livekit.EgressInfo_RoomComposite:
				info.RoomName = r.RoomComposite.RoomName
			case *livekit.EgressInfo_TrackComposite:
				info.RoomName = r.TrackComposite.RoomName
			case *livekit.EgressInfo_Track:
				info.RoomName = r.Track.RoomName
			}
		}
	}
}
