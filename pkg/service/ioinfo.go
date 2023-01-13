package service

import (
	"context"
	"errors"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/livekit/livekit-server/pkg/service/rpc"
	"github.com/livekit/livekit-server/pkg/telemetry"
	"github.com/livekit/protocol/egress"
	"github.com/livekit/protocol/ingress"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/psrpc"
)

type IOInfoService struct {
	psrpcServer  rpc.IOInfoServer
	es           EgressStore
	is           IngressStore
	telemetry    telemetry.TelemetryService
	ecDeprecated egress.RPCClient
	icDeprecated ingress.RPCClient
	shutdown     chan struct{}
}

func NewIOInfoService(
	nodeID livekit.NodeID,
	bus psrpc.MessageBus,
	es EgressStore,
	is IngressStore,
	ts telemetry.TelemetryService,
	ec egress.RPCClient,
	ic ingress.RPCClient,
) (*IOInfoService, error) {
	s := &IOInfoService{
		es:           es,
		is:           is,
		telemetry:    ts,
		ecDeprecated: ec,
		icDeprecated: ic,
		shutdown:     make(chan struct{}),
	}

	if bus != nil {
		psrpcServer, err := rpc.NewIOInfoServer(string(nodeID), s, bus)
		if err != nil {
			return nil, err
		}
		s.psrpcServer = psrpcServer
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

		go s.egressWorkerDeprecated()
		go s.ingressWorkerDeprecated()
	}

	return nil
}

func (s *IOInfoService) UpdateEgressInfo(ctx context.Context, info *livekit.EgressInfo) (*emptypb.Empty, error) {
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
			return nil, err
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
			return nil, err
		}
	}

	return &emptypb.Empty{}, nil
}

func (s *IOInfoService) GetIngressInfo(ctx context.Context, req *livekit.GetIngressInfoRequest) (*livekit.GetIngressInfoResponse, error) {
	info, err := s.loadIngressFromInfoRequest(req)
	if err != nil {
		return nil, err
	}

	return &livekit.GetIngressInfoResponse{Info: info}, nil
}

func (s *IOInfoService) loadIngressFromInfoRequest(req *livekit.GetIngressInfoRequest) (info *livekit.IngressInfo, err error) {
	if req.IngressId != "" {
		info, err = s.is.LoadIngress(context.Background(), req.IngressId)
	} else if req.StreamKey != "" {
		info, err = s.is.LoadIngressFromStreamKey(context.Background(), req.StreamKey)
	} else {
		err = errors.New("request needs to specity either IngressId or StreamKey")
	}
	return info, err
}

func (s *IOInfoService) UpdateIngressState(ctx context.Context, req *livekit.UpdateIngressStateRequest) (*emptypb.Empty, error) {
	if err := s.is.UpdateIngressState(ctx, req.IngressId, req.State); err != nil {
		logger.Errorw("could not update ingress", err)
		return nil, err
	}
	return &emptypb.Empty{}, nil
}

func (s *IOInfoService) Stop() {
	close(s.shutdown)

	if s.psrpcServer != nil {
		s.psrpcServer.Shutdown()
	}
}

// Deprecated
func (s *IOInfoService) egressWorkerDeprecated() error {
	if s.ecDeprecated == nil {
		return nil
	}

	go func() {
		sub, err := s.ecDeprecated.GetUpdateChannel(context.Background())
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
				_, err = s.UpdateEgressInfo(context.Background(), info)
				if err != nil {
					logger.Errorw("failed to update egress info", err)
				}

			case <-s.shutdown:
				_ = sub.Close()
				s.es.(*RedisStore).Stop()
				return
			}
		}
	}()

	return nil
}

// Deprecated
func (s *IOInfoService) ingressWorkerDeprecated() {
	if s.icDeprecated == nil {
		return
	}

	updates, err := s.icDeprecated.GetUpdateChannel(context.Background())
	if err != nil {
		logger.Errorw("failed to subscribe to results channel", err)
		return
	}

	entities, err := s.icDeprecated.GetEntityChannel(context.Background())
	if err != nil {
		logger.Errorw("failed to subscribe to entities channel", err)
		_ = updates.Close()
		return
	}

	updateChan := updates.Channel()
	entityChan := entities.Channel()
	for {
		select {
		case msg := <-updateChan:
			b := updates.Payload(msg)

			res := &livekit.UpdateIngressStateRequest{}
			if err = proto.Unmarshal(b, res); err != nil {
				logger.Errorw("failed to read results", err)
				continue
			}

			// save updated info to store
			err = s.is.UpdateIngressState(context.Background(), res.IngressId, res.State)
			if err != nil {
				logger.Errorw("could not update ingress", err)
			}

		case msg := <-entityChan:
			b := entities.Payload(msg)

			req := &livekit.GetIngressInfoRequest{}
			if err = proto.Unmarshal(b, req); err != nil {
				logger.Errorw("failed to read request", err)
				continue
			}

			info, err := s.loadIngressFromInfoRequest(req)
			if err != nil {
				logger.Errorw("failed to load ingress info", err)
				continue
			}
			err = s.icDeprecated.SendGetIngressInfoResponse(context.Background(), req, &livekit.GetIngressInfoResponse{Info: info}, err)
			if err != nil {
				logger.Errorw("could not send response", err)
			}

		case <-s.shutdown:
			_ = updates.Close()
			return
		}
	}
}
