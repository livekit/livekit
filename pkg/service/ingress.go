package service

import (
	"context"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/telemetry"
	"github.com/livekit/protocol/ingress"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/protocol/utils"
	"github.com/livekit/psrpc"
)

type IngressService struct {
	conf        *config.IngressConfig
	nodeID      livekit.NodeID
	bus         psrpc.MessageBus
	psrpcClient rpc.IngressClient
	store       IngressStore
	roomService livekit.RoomService
	telemetry   telemetry.TelemetryService
}

func NewIngressService(
	conf *config.IngressConfig,
	nodeID livekit.NodeID,
	bus psrpc.MessageBus,
	psrpcClient rpc.IngressClient,
	store IngressStore,
	rs livekit.RoomService,
	ts telemetry.TelemetryService,
) *IngressService {

	return &IngressService{
		conf:        conf,
		nodeID:      nodeID,
		bus:         bus,
		psrpcClient: psrpcClient,
		store:       store,
		roomService: rs,
		telemetry:   ts,
	}
}

func (s *IngressService) CreateIngress(ctx context.Context, req *livekit.CreateIngressRequest) (*livekit.IngressInfo, error) {
	fields := []interface{}{
		"inputType", req.InputType,
		"name", req.Name,
	}
	if req.RoomName != "" {
		fields = append(fields, "room", req.RoomName, "identity", req.ParticipantIdentity)
	}
	defer func() {
		AppendLogFields(ctx, fields...)
	}()

	var urlPrefix string
	switch req.InputType {
	case livekit.IngressInput_RTMP_INPUT:
		urlPrefix = s.conf.RTMPBaseURL
	case livekit.IngressInput_WHIP_INPUT:
		urlPrefix = s.conf.WHIPBaseURL
	default:
		return nil, ingress.ErrInvalidIngressType
	}

	ig, err := s.CreateIngressWithUrlPrefix(ctx, urlPrefix, req)
	if err != nil {
		return nil, err
	}
	fields = append(fields, "ingressID", ig.IngressId)

	return ig, nil
}

func (s *IngressService) CreateIngressWithUrlPrefix(ctx context.Context, urlPrefix string, req *livekit.CreateIngressRequest) (*livekit.IngressInfo, error) {
	err := EnsureIngressAdminPermission(ctx)
	if err != nil {
		return nil, twirpAuthError(err)
	}
	if s.store == nil {
		return nil, ErrIngressNotConnected
	}

	sk := utils.NewGuid("")

	info := &livekit.IngressInfo{
		IngressId:           utils.NewGuid(utils.IngressPrefix),
		Name:                req.Name,
		StreamKey:           sk,
		Url:                 urlPrefix,
		InputType:           req.InputType,
		Audio:               req.Audio,
		Video:               req.Video,
		BypassTranscoding:   req.BypassTranscoding,
		RoomName:            req.RoomName,
		ParticipantIdentity: req.ParticipantIdentity,
		ParticipantName:     req.ParticipantName,
		Reusable:            req.InputType == livekit.IngressInput_RTMP_INPUT,
		State:               &livekit.IngressState{},
	}

	if err := ingress.ValidateForSerialization(info); err != nil {
		return nil, err
	}

	if err = s.store.StoreIngress(ctx, info); err != nil {
		logger.Errorw("could not write ingress info", err)
		return nil, err
	}

	return info, nil
}

func updateInfoUsingRequest(req *livekit.UpdateIngressRequest, info *livekit.IngressInfo) error {
	if req.Name != "" {
		info.Name = req.Name
	}
	if req.RoomName != "" {
		info.RoomName = req.RoomName
	}
	if req.ParticipantIdentity != "" {
		info.ParticipantIdentity = req.ParticipantIdentity
	}
	if req.ParticipantName != "" {
		info.ParticipantName = req.ParticipantName
	}
	if req.BypassTranscoding != nil {
		info.BypassTranscoding = *req.BypassTranscoding
	}
	if req.Audio != nil {
		info.Audio = req.Audio
	}
	if req.Video != nil {
		info.Video = req.Video
	}

	if err := ingress.ValidateForSerialization(info); err != nil {
		return err
	}

	return nil
}

func (s *IngressService) UpdateIngress(ctx context.Context, req *livekit.UpdateIngressRequest) (*livekit.IngressInfo, error) {
	fields := []interface{}{
		"ingress", req.IngressId,
		"name", req.Name,
	}
	if req.RoomName != "" {
		fields = append(fields, "room", req.RoomName, "identity", req.ParticipantIdentity)
	}
	AppendLogFields(ctx, fields...)
	err := EnsureIngressAdminPermission(ctx)
	if err != nil {
		return nil, twirpAuthError(err)
	}

	if s.psrpcClient == nil {
		return nil, ErrIngressNotConnected
	}

	info, err := s.store.LoadIngress(ctx, req.IngressId)
	if err != nil {
		logger.Errorw("could not load ingress info", err)
		return nil, err
	}

	switch info.State.Status {
	case livekit.IngressState_ENDPOINT_ERROR:
		info.State.Status = livekit.IngressState_ENDPOINT_INACTIVE
		err = s.store.UpdateIngressState(ctx, req.IngressId, info.State)
		if err != nil {
			logger.Warnw("could not store ingress state", err)
		}
		fallthrough

	case livekit.IngressState_ENDPOINT_INACTIVE:
		err = updateInfoUsingRequest(req, info)
		if err != nil {
			return nil, err
		}

	case livekit.IngressState_ENDPOINT_BUFFERING,
		livekit.IngressState_ENDPOINT_PUBLISHING:
		err := updateInfoUsingRequest(req, info)
		if err != nil {
			return nil, err
		}

		// Do not store the returned state as the ingress service will do it
		if _, err = s.psrpcClient.UpdateIngress(ctx, req.IngressId, req); err != nil {
			logger.Warnw("could not update active ingress", err)
		}
	}

	err = s.store.UpdateIngress(ctx, info)
	if err != nil {
		logger.Errorw("could not update ingress info", err)
		return nil, err
	}

	return info, nil
}

func (s *IngressService) ListIngress(ctx context.Context, req *livekit.ListIngressRequest) (*livekit.ListIngressResponse, error) {
	AppendLogFields(ctx, "room", req.RoomName)
	err := EnsureIngressAdminPermission(ctx)
	if err != nil {
		return nil, twirpAuthError(err)
	}
	if s.store == nil {
		return nil, ErrIngressNotConnected
	}

	infos, err := s.store.ListIngress(ctx, livekit.RoomName(req.RoomName))
	if err != nil {
		logger.Errorw("could not list ingress info", err)
		return nil, err
	}

	return &livekit.ListIngressResponse{Items: infos}, nil
}

func (s *IngressService) DeleteIngress(ctx context.Context, req *livekit.DeleteIngressRequest) (*livekit.IngressInfo, error) {
	AppendLogFields(ctx, "ingressID", req.IngressId)
	if err := EnsureIngressAdminPermission(ctx); err != nil {
		return nil, twirpAuthError(err)
	}

	if s.psrpcClient == nil {
		return nil, ErrIngressNotConnected
	}

	info, err := s.store.LoadIngress(ctx, req.IngressId)
	if err != nil {
		return nil, err
	}

	switch info.State.Status {
	case livekit.IngressState_ENDPOINT_BUFFERING,
		livekit.IngressState_ENDPOINT_PUBLISHING:
		if _, err = s.psrpcClient.DeleteIngress(ctx, req.IngressId, req); err != nil {
			logger.Warnw("could not stop active ingress", err)
		}
	}

	err = s.store.DeleteIngress(ctx, info)
	if err != nil {
		logger.Errorw("could not delete ingress info", err)
		return nil, err
	}

	info.State.Status = livekit.IngressState_ENDPOINT_INACTIVE
	return info, nil
}
