package service

import (
	"context"
	"errors"

	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/livekit/livekit-server/pkg/telemetry"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/psrpc"
)

type IOInfoService struct {
	ioServer  rpc.IOInfoServer
	es        EgressStore
	is        IngressStore
	telemetry telemetry.TelemetryService
	shutdown  chan struct{}
}

func NewIOInfoService(
	nodeID livekit.NodeID,
	bus psrpc.MessageBus,
	es EgressStore,
	is IngressStore,
	ts telemetry.TelemetryService,
) (*IOInfoService, error) {
	s := &IOInfoService{
		es:        es,
		is:        is,
		telemetry: ts,
		shutdown:  make(chan struct{}),
	}

	if bus != nil {
		ioServer, err := rpc.NewIOInfoServer(string(nodeID), s, bus)
		if err != nil {
			return nil, err
		}
		s.ioServer = ioServer
	}

	return s, nil
}

func (s *IOInfoService) Start() error {
	if s.es != nil {
		rs := s.es.(*RedisStore)
		err := rs.Start()
		if err != nil {
			logger.Errorw("failed to start redis egress worker", err)
			return err
		}
	}

	return nil
}

func (s *IOInfoService) UpdateEgressInfo(ctx context.Context, info *livekit.EgressInfo) (*emptypb.Empty, error) {
	err := s.es.UpdateEgress(ctx, info)

	switch info.Status {
	case livekit.EgressStatus_EGRESS_ACTIVE:
		s.telemetry.EgressUpdated(ctx, info)

	case livekit.EgressStatus_EGRESS_COMPLETE,
		livekit.EgressStatus_EGRESS_FAILED,
		livekit.EgressStatus_EGRESS_ABORTED,
		livekit.EgressStatus_EGRESS_LIMIT_REACHED:

		// log results
		if info.Error != "" {
			logger.Errorw("egress failed", errors.New(info.Error), "egressID", info.EgressId)
		} else {
			logger.Infow("egress ended", "egressID", info.EgressId)
		}

		s.telemetry.EgressEnded(ctx, info)
	}
	if err != nil {
		logger.Errorw("could not update egress", err)
		return nil, err
	}

	return &emptypb.Empty{}, nil
}

func (s *IOInfoService) GetIngressInfo(ctx context.Context, req *rpc.GetIngressInfoRequest) (*rpc.GetIngressInfoResponse, error) {
	info, err := s.loadIngressFromInfoRequest(req)
	if err != nil {
		return nil, err
	}

	return &rpc.GetIngressInfoResponse{Info: info}, nil
}

func (s *IOInfoService) loadIngressFromInfoRequest(req *rpc.GetIngressInfoRequest) (info *livekit.IngressInfo, err error) {
	if req.IngressId != "" {
		info, err = s.is.LoadIngress(context.Background(), req.IngressId)
	} else if req.StreamKey != "" {
		info, err = s.is.LoadIngressFromStreamKey(context.Background(), req.StreamKey)
	} else {
		err = errors.New("request needs to specify either IngressId or StreamKey")
	}
	return info, err
}

func (s *IOInfoService) UpdateIngressState(ctx context.Context, req *rpc.UpdateIngressStateRequest) (*emptypb.Empty, error) {
	if err := s.is.UpdateIngressState(ctx, req.IngressId, req.State); err != nil {
		logger.Errorw("could not update ingress", err)
		return nil, err
	}
	return &emptypb.Empty{}, nil
}

func (s *IOInfoService) Stop() {
	close(s.shutdown)

	if s.ioServer != nil {
		s.ioServer.Shutdown()
	}
}
