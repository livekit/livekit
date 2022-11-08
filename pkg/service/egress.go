package service

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/livekit/livekit-server/pkg/rtc"
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
	launcher    rtc.EgressLauncher
	shutdown    chan struct{}
}

type egressLauncher struct {
	rpcClient egress.RPCClient
	es        EgressStore
	telemetry telemetry.TelemetryService
}

func NewEgressLauncher(rpcClient egress.RPCClient, es EgressStore, ts telemetry.TelemetryService) rtc.EgressLauncher {
	if rpcClient == nil {
		return nil
	}

	return &egressLauncher{
		rpcClient: rpcClient,
		es:        es,
		telemetry: ts,
	}
}

func NewEgressService(
	rpcClient egress.RPCClient,
	store ServiceStore,
	es EgressStore,
	rs livekit.RoomService,
	ts telemetry.TelemetryService,
	launcher rtc.EgressLauncher,
) *EgressService {
	return &EgressService{
		rpcClient:   rpcClient,
		store:       store,
		es:          es,
		roomService: rs,
		telemetry:   ts,
		launcher:    launcher,
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

func (s *EgressService) StartWebEgress(ctx context.Context, req *livekit.WebEgressRequest) (*livekit.EgressInfo, error) {
	return s.StartEgress(ctx, "", &livekit.StartEgressRequest{
		Request: &livekit.StartEgressRequest_Web{
			Web: req,
		},
	})
}

func (s *EgressService) StartEgress(ctx context.Context, roomName livekit.RoomName, req *livekit.StartEgressRequest) (*livekit.EgressInfo, error) {
	if err := EnsureRecordPermission(ctx); err != nil {
		return nil, twirpAuthError(err)
	} else if s.launcher == nil {
		return nil, ErrEgressNotConnected
	}

	if roomName != "" {
		room, _, err := s.store.LoadRoom(ctx, roomName, false)
		if err != nil {
			return nil, err
		}
		req.RoomId = room.Sid
	}

	return s.launcher.StartEgress(ctx, req)
}

func (s *egressLauncher) StartEgress(ctx context.Context, req *livekit.StartEgressRequest) (*livekit.EgressInfo, error) {
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
