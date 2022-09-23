package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/telemetry"
	"github.com/livekit/protocol/ingress"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils"
)

var (
	initialTimeout = time.Second * 3
	retryTimeout   = time.Minute * 1
)

type IngressService struct {
	conf        *config.IngressConfig
	rpcClient   ingress.RPCClient
	store       IngressStore
	roomService livekit.RoomService
	telemetry   telemetry.TelemetryService
	shutdown    chan struct{}
}

func NewIngressService(
	conf *config.IngressConfig,
	rpcClient ingress.RPCClient,
	store IngressStore,
	rs livekit.RoomService,
	ts telemetry.TelemetryService,
) *IngressService {

	return &IngressService{
		conf:        conf,
		rpcClient:   rpcClient,
		store:       store,
		roomService: rs,
		telemetry:   ts,
		shutdown:    make(chan struct{}),
	}
}

func (s *IngressService) Start() {
	if s.rpcClient != nil {
		go s.updateWorker()
		go s.entitiesWorker()
	}
}

func (s *IngressService) Stop() {
	close(s.shutdown)
}

func (s *IngressService) CreateIngress(ctx context.Context, req *livekit.CreateIngressRequest) (*livekit.IngressInfo, error) {
	return s.CreateIngressWithUrlPrefix(ctx, s.conf.RTMPBaseURL, req)
}

func (s *IngressService) CreateIngressWithUrlPrefix(ctx context.Context, urlPrefix string, req *livekit.CreateIngressRequest) (*livekit.IngressInfo, error) {
	roomName, err := EnsureJoinPermission(ctx)
	if err != nil {
		return nil, twirpAuthError(err)
	}
	if req.RoomName != "" && req.RoomName != string(roomName) {
		return nil, twirpAuthError(ErrPermissionDenied)
	}

	sk := utils.NewGuid("")

	info := &livekit.IngressInfo{
		IngressId:           utils.NewGuid(utils.IngressPrefix),
		Name:                req.Name,
		StreamKey:           sk,
		Url:                 newRtmpUrl(urlPrefix, sk),
		InputType:           req.InputType,
		Audio:               req.Audio,
		Video:               req.Video,
		RoomName:            req.RoomName,
		ParticipantIdentity: req.ParticipantIdentity,
		ParticipantName:     req.ParticipantName,
		Reusable:            req.InputType == livekit.IngressInput_RTMP_INPUT,
		State: &livekit.IngressState{
			Status: livekit.IngressState_ENDPOINT_INACTIVE,
		},
	}

	if err := s.store.StoreIngress(ctx, info); err != nil {
		logger.Errorw("could not write ingress info", err)
		return nil, err
	}

	return info, nil
}

func (s *IngressService) sendRPCWithRetry(ctx context.Context, req *livekit.IngressRequest) (*livekit.IngressInfo, error) {
	type result struct {
		info *livekit.IngressInfo
		err  error
	}

	resChan := make(chan result, 1)

	go func() {
		cctx, _ := context.WithTimeout(context.Background(), retryTimeout)

		for {
			select {
			case <-cctx.Done():
				resChan <- result{nil, ingress.ErrNoResponse}
				return
			default:
			}

			i, err := s.rpcClient.SendRequest(cctx, req)
			if err != ingress.ErrNoResponse {
				resChan <- result{i, err}
			}
		}
	}()

	select {
	case res := <-resChan:
		return res.info, res.err
	case <-time.After(initialTimeout):
		return nil, ingress.ErrNoResponse
	}
}

func (s *IngressService) UpdateIngress(ctx context.Context, req *livekit.UpdateIngressRequest) (*livekit.IngressInfo, error) {
	roomName, err := EnsureJoinPermission(ctx)
	if err != nil {
		return nil, twirpAuthError(err)
	}
	if req.RoomName != "" && req.RoomName != string(roomName) {
		return nil, twirpAuthError(ErrPermissionDenied)
	}

	if s.rpcClient == nil {
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
		fallthrough

	case livekit.IngressState_ENDPOINT_INACTIVE:
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
		if req.Audio != nil {
			info.Audio = req.Audio
		}
		if req.Video != nil {
			info.Video = req.Video
		}

	case livekit.IngressState_ENDPOINT_BUFFERING,
		livekit.IngressState_ENDPOINT_PUBLISHING:
		i, err := s.sendRPCWithRetry(ctx, &livekit.IngressRequest{
			IngressId: req.IngressId,
			Request:   &livekit.IngressRequest_Update{Update: req},
		})
		if err != nil {
			logger.Warnw("could not update active ingress", err)
		} else {
			info = i
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
	roomName, err := EnsureJoinPermission(ctx)
	if err != nil {
		return nil, twirpAuthError(err)
	}
	if req.RoomName != "" && req.RoomName != string(roomName) {
		return nil, twirpAuthError(ErrPermissionDenied)
	}

	infos, err := s.store.ListIngress(ctx, livekit.RoomName(req.RoomName))
	if err != nil {
		logger.Errorw("could not list ingress info", err)
		return nil, err
	}

	return &livekit.ListIngressResponse{Items: infos}, nil
}

func (s *IngressService) DeleteIngress(ctx context.Context, req *livekit.DeleteIngressRequest) (*livekit.IngressInfo, error) {
	if _, err := EnsureJoinPermission(ctx); err != nil {
		return nil, twirpAuthError(err)
	}

	if s.rpcClient == nil {
		return nil, ErrIngressNotConnected
	}

	info, err := s.store.LoadIngress(ctx, req.IngressId)
	if err != nil {
		return nil, err
	}

	switch info.State.Status {
	case livekit.IngressState_ENDPOINT_BUFFERING,
		livekit.IngressState_ENDPOINT_PUBLISHING:
		i, err := s.sendRPCWithRetry(ctx, &livekit.IngressRequest{
			IngressId: req.IngressId,
			Request:   &livekit.IngressRequest_Delete{Delete: req},
		})
		if err != nil {
			logger.Warnw("could not stop active ingress", err)
		} else {
			info = i
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

func (s *IngressService) updateWorker() {
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

			res := &livekit.IngressInfo{}
			if err = proto.Unmarshal(b, res); err != nil {
				logger.Errorw("failed to read results", err)
				continue
			}

			// save updated info to store
			err = s.store.UpdateIngress(context.Background(), res)
			if err != nil {
				logger.Errorw("could not update ingress", err)
			}

		case <-s.shutdown:
			_ = sub.Close()
			return
		}
	}
}

func (s *IngressService) entitiesWorker() {
	sub, err := s.rpcClient.GetEntityChannel(context.Background())
	if err != nil {
		logger.Errorw("failed to subscribe to entities channel", err)
		return
	}

	resChan := sub.Channel()
	for {
		select {
		case msg := <-resChan:
			b := sub.Payload(msg)

			req := &livekit.GetIngressInfoRequest{}
			if err = proto.Unmarshal(b, req); err != nil {
				logger.Errorw("failed to read request", err)
				continue
			}

			var info *livekit.IngressInfo
			var err error
			if req.IngressId != "" {
				info, err = s.store.LoadIngress(context.Background(), req.IngressId)
			} else if req.StreamKey != "" {
				info, err = s.store.LoadIngressFromStreamKey(context.Background(), req.StreamKey)
			} else {
				err = errors.New("request needs to specity either IngressId or StreamKey")
			}
			err = s.rpcClient.SendGetIngressInfoResponse(context.Background(), req, &livekit.GetIngressInfoResponse{Info: info}, err)
			if err != nil {
				logger.Errorw("could not send response", err)
			}

		case <-s.shutdown:
			_ = sub.Close()
			return
		}
	}
}

func newRtmpUrl(baseUrl string, ingressId string) string {
	return fmt.Sprintf("%s/%s", baseUrl, ingressId)
}
