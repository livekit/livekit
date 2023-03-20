package service

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/twitchtv/twirp"

	"github.com/livekit/livekit-server/pkg/rtc"
	"github.com/livekit/livekit-server/pkg/telemetry"
	"github.com/livekit/protocol/egress"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/protocol/utils"
)

type EgressService struct {
	psrpcClient      rpc.EgressClient
	clientDeprecated egress.RPCClient
	store            ServiceStore
	es               EgressStore
	roomService      livekit.RoomService
	telemetry        telemetry.TelemetryService
	launcher         rtc.EgressLauncher
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

func (s *EgressService) StartRoomCompositeEgress(ctx context.Context, req *livekit.RoomCompositeEgressRequest) (*livekit.EgressInfo, error) {
	fields := []interface{}{"room", req.RoomName, "baseUrl", req.CustomBaseUrl}
	if t := reflect.TypeOf(req.Output); t != nil {
		fields = append(fields, "outputType", t.String())
	}
	defer func() {
		AppendLogFields(ctx, fields...)
	}()
	ei, err := s.startEgress(ctx, livekit.RoomName(req.RoomName), &rpc.StartEgressRequest{
		Request: &rpc.StartEgressRequest_RoomComposite{
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
		"room", req.RoomName, "audioTrackID", req.AudioTrackId, "videoTrackID", req.VideoTrackId,
	}
	if t := reflect.TypeOf(req.Output); t != nil {
		fields = append(fields, "outputType", t.String())
	}
	defer func() {
		AppendLogFields(ctx, fields...)
	}()
	ei, err := s.startEgress(ctx, livekit.RoomName(req.RoomName), &rpc.StartEgressRequest{
		Request: &rpc.StartEgressRequest_TrackComposite{
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
	fields := []interface{}{"room", req.RoomName, "trackID", req.TrackId}
	if t := reflect.TypeOf(req.Output); t != nil {
		fields = append(fields, "outputType", t.String())
	}
	defer func() {
		AppendLogFields(ctx, fields...)
	}()
	ei, err := s.startEgress(ctx, livekit.RoomName(req.RoomName), &rpc.StartEgressRequest{
		Request: &rpc.StartEgressRequest_Track{
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
	fields := []interface{}{"url", req.Url}
	if t := reflect.TypeOf(req.Output); t != nil {
		fields = append(fields, "outputType", t.String())
	}
	defer func() {
		AppendLogFields(ctx, fields...)
	}()
	ei, err := s.startEgress(ctx, "", &rpc.StartEgressRequest{
		Request: &rpc.StartEgressRequest_Web{
			Web: req,
		},
	})
	if err != nil {
		return nil, err
	}
	fields = append(fields, "egressID", ei.EgressId)
	return ei, err
}

func (s *EgressService) startEgress(ctx context.Context, roomName livekit.RoomName, req *rpc.StartEgressRequest) (*livekit.EgressInfo, error) {
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

func (s *egressLauncher) StartEgress(ctx context.Context, req *rpc.StartEgressRequest) (*livekit.EgressInfo, error) {
	return s.StartEgressWithClusterId(ctx, "", req)
}
func (s *egressLauncher) StartEgressWithClusterId(ctx context.Context, clusterId string, req *rpc.StartEgressRequest) (*livekit.EgressInfo, error) {
	var info *livekit.EgressInfo
	var err error

	// Ensure we have an Egress ID
	if req.EgressId == "" {
		req.EgressId = utils.NewGuid(utils.EgressPrefix)
	}

	if s.psrpcClient != nil {
		info, err = s.psrpcClient.StartEgress(ctx, clusterId, req)
	} else {
		logger.Warnw("Using deprecated egress client. Upgrade egress to v1.5.6+ and use egress:use_psrpc:true in your livekit config", nil)
		// SendRequest will transform rpc.StartEgressRequest into deprecated livekit.StartEgressRequest
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

	race := rpc.NewRace[livekit.EgressInfo](ctx)
	if s.clientDeprecated != nil {
		race.Go(func(ctx context.Context) (*livekit.EgressInfo, error) {
			return s.clientDeprecated.SendRequest(ctx, &livekit.EgressRequest{
				EgressId: req.EgressId,
				Request: &livekit.EgressRequest_UpdateStream{
					UpdateStream: req,
				},
			})
		})
	}
	if s.psrpcClient != nil {
		race.Go(func(ctx context.Context) (*livekit.EgressInfo, error) {
			return s.psrpcClient.UpdateStream(ctx, req.EgressId, req)
		})
	}
	_, info, err := race.Wait()
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

	var items []*livekit.EgressInfo
	if req.EgressId != "" {
		info, err := s.es.LoadEgress(ctx, req.EgressId)
		if err != nil {
			return nil, err
		}

		if !req.Active || int32(info.Status) < int32(livekit.EgressStatus_EGRESS_COMPLETE) {
			items = []*livekit.EgressInfo{info}
		}
	} else {
		var err error
		items, err = s.es.ListEgress(ctx, livekit.RoomName(req.RoomName), req.Active)
		if err != nil {
			return nil, err
		}
	}

	return &livekit.ListEgressResponse{Items: items}, nil
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
			return nil, twirp.NewError(twirp.FailedPrecondition, fmt.Sprintf("egress with status %s cannot be stopped", info.Status.String()))
		}
	}

	race := rpc.NewRace[livekit.EgressInfo](ctx)
	if s.clientDeprecated != nil {
		race.Go(func(ctx context.Context) (*livekit.EgressInfo, error) {
			return s.clientDeprecated.SendRequest(ctx, &livekit.EgressRequest{
				EgressId: req.EgressId,
				Request: &livekit.EgressRequest_Stop{
					Stop: req,
				},
			})
		})
	}
	if s.psrpcClient != nil {
		race.Go(func(ctx context.Context) (*livekit.EgressInfo, error) {
			return s.psrpcClient.StopEgress(ctx, req.EgressId, req)
		})
	}
	_, info, err = race.Wait()
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
