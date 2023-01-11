package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/livekit/livekit-server/pkg/rtc"
	"github.com/livekit/livekit-server/pkg/service/rpc"
	"github.com/livekit/protocol/egress"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils"

	"github.com/livekit/livekit-server/pkg/telemetry"
)

type EgressService struct {
	psrpcClient      rpc.EgressClient
	clientDeprecated egress.RPCClient
	store            ServiceStore
	es               EgressStore
	roomService      livekit.RoomService
	telemetry        telemetry.TelemetryService
	launcher         rtc.EgressLauncher
	shutdown         chan struct{}
}

type egressLauncher struct {
	psrpcClient      rpc.EgressClient
	clientDeprecated egress.RPCClient
	es               EgressStore
	telemetry        telemetry.TelemetryService
}

func NewEgressLauncher(
	psrpcClient rpc.EgressClient,
	clientDeprecated egress.RPCClient,
	es EgressStore,
	ts telemetry.TelemetryService) rtc.EgressLauncher {
	if psrpcClient == nil && clientDeprecated == nil {
		return nil
	}

	return &egressLauncher{
		psrpcClient:      psrpcClient,
		clientDeprecated: clientDeprecated,
		es:               es,
		telemetry:        ts,
	}
}

func NewEgressService(
	psrpcClient rpc.EgressClient,
	clientDeprecated egress.RPCClient,
	store ServiceStore,
	es EgressStore,
	rs livekit.RoomService,
	ts telemetry.TelemetryService,
	launcher rtc.EgressLauncher,
) *EgressService {
	return &EgressService{
		psrpcClient:      psrpcClient,
		clientDeprecated: clientDeprecated,
		store:            store,
		es:               es,
		roomService:      rs,
		telemetry:        ts,
		launcher:         launcher,
	}
}

func (s *EgressService) Start() error {
	if s.shutdown != nil {
		return nil
	}

	s.shutdown = make(chan struct{})
	if (s.psrpcClient != nil || s.clientDeprecated != nil) && s.es != nil {
		return s.startWorker()
	}

	return nil
}

func (s *EgressService) Stop() {
	close(s.shutdown)
}

func (s *EgressService) StartRoomCompositeEgress(ctx context.Context, req *livekit.RoomCompositeEgressRequest) (*livekit.EgressInfo, error) {
	fields := []interface{}{"room", req.RoomName, "outputType", reflect.TypeOf(req.Output).String(), "baseUrl", req.CustomBaseUrl}
	defer func() {
		AppendLogFields(ctx, fields...)
	}()
	ei, err := s.startEgress(ctx, livekit.RoomName(req.RoomName), &livekit.StartEgressRequest{
		Request: &livekit.StartEgressRequest_RoomComposite{
			RoomComposite: req,
		},
	})
	if err != nil {
		return nil, err
	}
	fields = append(fields, "egressID", ei.EgressId)
	return ei, err
}

func (s *EgressService) StartTrackCompositeEgress(ctx context.Context, req *livekit.TrackCompositeEgressRequest) (*livekit.EgressInfo, error) {
	fields := []interface{}{
		"room", req.RoomName, "outputType", reflect.TypeOf(req.Output).String(), "audioTrackID", req.AudioTrackId, "videoTrackID", req.VideoTrackId,
	}
	defer func() {
		AppendLogFields(ctx, fields...)
	}()
	ei, err := s.startEgress(ctx, livekit.RoomName(req.RoomName), &livekit.StartEgressRequest{
		Request: &livekit.StartEgressRequest_TrackComposite{
			TrackComposite: req,
		},
	})
	if err != nil {
		return nil, err
	}
	fields = append(fields, "egressID", ei.EgressId)
	return ei, err
}

func (s *EgressService) StartTrackEgress(ctx context.Context, req *livekit.TrackEgressRequest) (*livekit.EgressInfo, error) {
	fields := []interface{}{"room", req.RoomName, "trackID", req.TrackId, "outputType", reflect.TypeOf(req.Output).String()}
	defer func() {
		AppendLogFields(ctx, fields...)
	}()
	ei, err := s.startEgress(ctx, livekit.RoomName(req.RoomName), &livekit.StartEgressRequest{
		Request: &livekit.StartEgressRequest_Track{
			Track: req,
		},
	})
	if err != nil {
		return nil, err
	}
	fields = append(fields, "egressID", ei.EgressId)
	return ei, err
}

func (s *EgressService) StartWebEgress(ctx context.Context, req *livekit.WebEgressRequest) (*livekit.EgressInfo, error) {
	fields := []interface{}{"url", req.Url, "outputType", reflect.TypeOf(req.Output).String()}
	defer func() {
		AppendLogFields(ctx, fields...)
	}()
	ei, err := s.startEgress(ctx, "", &livekit.StartEgressRequest{
		Request: &livekit.StartEgressRequest_Web{
			Web: req,
		},
	})
	if err != nil {
		return nil, err
	}
	fields = append(fields, "egressID", ei.EgressId)
	return ei, err
}

func (s *EgressService) startEgress(ctx context.Context, roomName livekit.RoomName, req *livekit.StartEgressRequest) (*livekit.EgressInfo, error) {
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
	var info *livekit.EgressInfo
	var err error

	// Ensure we have a Egress ID
	if req.EgressId == "" {
		req.EgressId = utils.NewGuid(utils.EgressPrefix)
	}

	if s.psrpcClient != nil {
		info, err = s.psrpcClient.StartEgress(ctx, req)
	} else {
		logger.Infow("using deprecated egress client")
		info, err = s.clientDeprecated.SendRequest(ctx, req)
	}
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
	AppendLogFields(ctx, "egressID", req.EgressId, "layout", req.Layout)
	if err := EnsureRecordPermission(ctx); err != nil {
		return nil, twirpAuthError(err)
	}
	if s.psrpcClient == nil && s.clientDeprecated == nil {
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
	AppendLogFields(ctx, "egressID", req.EgressId, "addUrls", req.AddOutputUrls, "removeUrls", req.RemoveOutputUrls)
	if err := EnsureRecordPermission(ctx); err != nil {
		return nil, twirpAuthError(err)
	}

	if s.psrpcClient == nil && s.clientDeprecated == nil {
		return nil, ErrEgressNotConnected
	}

	f0 := func() (*livekit.EgressInfo, error) {
		return s.clientDeprecated.SendRequest(ctx, &livekit.EgressRequest{
			EgressId: req.EgressId,
			Request: &livekit.EgressRequest_UpdateStream{
				UpdateStream: req,
			},
		})
	}
	f1 := func() (*livekit.EgressInfo, error) {
		return s.psrpcClient.UpdateStream(ctx, req.EgressId, req)
	}

	info, err := s.getFirst(f0, f1)
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
	if req.RoomName != "" {
		AppendLogFields(ctx, "room", req.RoomName)
	}
	if err := EnsureRecordPermission(ctx); err != nil {
		return nil, twirpAuthError(err)
	}
	if s.psrpcClient == nil && s.clientDeprecated == nil {
		return nil, ErrEgressNotConnected
	}

	infos, err := s.es.ListEgress(ctx, livekit.RoomName(req.RoomName))
	if err != nil {
		return nil, err
	}

	return &livekit.ListEgressResponse{Items: infos}, nil
}

func (s *EgressService) StopEgress(ctx context.Context, req *livekit.StopEgressRequest) (*livekit.EgressInfo, error) {
	AppendLogFields(ctx, "egressID", req.EgressId)
	if err := EnsureRecordPermission(ctx); err != nil {
		return nil, twirpAuthError(err)
	}

	if s.psrpcClient == nil && s.clientDeprecated == nil {
		return nil, ErrEgressNotConnected
	}

	info, err := s.es.LoadEgress(ctx, req.EgressId)
	if err != nil {
		return nil, err
	} else {
		if info.Status != livekit.EgressStatus_EGRESS_STARTING &&
			info.Status != livekit.EgressStatus_EGRESS_ACTIVE {
			return nil, fmt.Errorf("egress with status %s cannot be stopped", info.Status.String())
		}
	}

	f0 := func() (*livekit.EgressInfo, error) {
		return s.clientDeprecated.SendRequest(ctx, &livekit.EgressRequest{
			EgressId: req.EgressId,
			Request: &livekit.EgressRequest_Stop{
				Stop: req,
			},
		})
	}
	f1 := func() (*livekit.EgressInfo, error) {
		return s.psrpcClient.StopEgress(ctx, req.EgressId, req)
	}

	info, err = s.getFirst(f0, f1)
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
	err := rs.Start()
	if err != nil {
		logger.Errorw("failed to start redis egress worker", err)
		return err
	}

	if s.psrpcClient != nil {
		go func() {
			sub, err := s.psrpcClient.SubscribeInfoUpdate(context.Background())
			if err != nil {
				logger.Errorw("failed to subscribe", err)
			}

			for {
				select {
				case info := <-sub.Channel():
					s.handleUpdate(info)
				case <-s.shutdown:
					_ = sub.Close()
					return
				}
			}
		}()
	}

	if s.clientDeprecated != nil {
		go func() {
			sub, err := s.clientDeprecated.GetUpdateChannel(context.Background())
			if err != nil {
				logger.Errorw("failed to subscribe to results channel", err)
			}

			resChan := sub.Channel()
			for {
				select {
				case msg := <-resChan:
					b := sub.Payload(msg)
					info := &livekit.EgressInfo{}
					if err = proto.Unmarshal(b, info); err != nil {
						logger.Errorw("failed to read results", err)
						continue
					}
					s.handleUpdate(info)
				case <-s.shutdown:
					_ = sub.Close()
					rs.Stop()
					return
				}
			}
		}()
	}

	return nil
}

func (s *EgressService) handleUpdate(info *livekit.EgressInfo) {
	switch info.Status {
	case livekit.EgressStatus_EGRESS_COMPLETE,
		livekit.EgressStatus_EGRESS_FAILED,
		livekit.EgressStatus_EGRESS_ABORTED:

		// make sure endedAt is set so it eventually gets deleted
		if info.EndedAt == 0 {
			info.EndedAt = time.Now().UnixNano()
		}

		if err := s.es.UpdateEgress(context.Background(), info); err != nil {
			logger.Errorw("could not update egress", err)
		}

		// log results
		if info.Error != "" {
			logger.Errorw("egress failed", errors.New(info.Error), "egressID", info.EgressId)
		} else {
			logger.Infow("egress ended", "egressID", info.EgressId)
		}

		s.telemetry.EgressEnded(context.Background(), info)

	default:
		if err := s.es.UpdateEgress(context.Background(), info); err != nil {
			logger.Errorw("could not update egress", err)
		}
	}
}

// TODO: remove in future version
func (s *EgressService) getFirst(f0, f1 func() (*livekit.EgressInfo, error)) (*livekit.EgressInfo, error) {
	if s.clientDeprecated == nil {
		return f1()
	}
	if s.psrpcClient == nil {
		return f0()
	}

	type res struct {
		info *livekit.EgressInfo
		err  error
	}
	v0 := make(chan *res, 1)
	v1 := make(chan *res, 1)

	go func() {
		info, err := f0()
		v0 <- &res{
			info: info,
			err:  err,
		}
	}()

	go func() {
		info, err := f1()
		v1 <- &res{
			info: info,
			err:  err,
		}
	}()

	select {
	case r := <-v0:
		return r.info, r.err
	case r := <-v1:
		return r.info, r.err
	}
}
