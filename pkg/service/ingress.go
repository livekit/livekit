// Copyright 2023 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package service

import (
	"context"
	"fmt"
	"net/url"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/telemetry"
	"github.com/livekit/protocol/ingress"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/protocol/utils"
	"github.com/livekit/protocol/utils/guid"
	"github.com/livekit/psrpc"
)

type IngressLauncher interface {
	LaunchPullIngress(ctx context.Context, info *livekit.IngressInfo) (*livekit.IngressInfo, error)
}

type IngressService struct {
	conf        *config.IngressConfig
	nodeID      livekit.NodeID
	bus         psrpc.MessageBus
	psrpcClient rpc.IngressClient
	store       IngressStore
	io          IOClient
	telemetry   telemetry.TelemetryService
	launcher    IngressLauncher
}

func NewIngressServiceWithIngressLauncher(
	conf *config.IngressConfig,
	nodeID livekit.NodeID,
	bus psrpc.MessageBus,
	psrpcClient rpc.IngressClient,
	store IngressStore,
	io IOClient,
	ts telemetry.TelemetryService,
	launcher IngressLauncher,
) *IngressService {

	return &IngressService{
		conf:        conf,
		nodeID:      nodeID,
		bus:         bus,
		psrpcClient: psrpcClient,
		store:       store,
		io:          io,
		telemetry:   ts,
		launcher:    launcher,
	}
}

func NewIngressService(
	conf *config.IngressConfig,
	nodeID livekit.NodeID,
	bus psrpc.MessageBus,
	psrpcClient rpc.IngressClient,
	store IngressStore,
	io IOClient,
	ts telemetry.TelemetryService,
) *IngressService {
	s := NewIngressServiceWithIngressLauncher(conf, nodeID, bus, psrpcClient, store, io, ts, nil)

	s.launcher = s

	return s
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

	var url string
	switch req.InputType {
	case livekit.IngressInput_RTMP_INPUT:
		url = s.conf.RTMPBaseURL
	case livekit.IngressInput_WHIP_INPUT:
		url = s.conf.WHIPBaseURL
	case livekit.IngressInput_URL_INPUT:
	default:
		return nil, ingress.ErrInvalidIngressType
	}

	ig, err := s.CreateIngressWithUrl(ctx, url, req)
	if err != nil {
		return nil, err
	}
	fields = append(fields, "ingressID", ig.IngressId)

	return ig, nil
}

func (s *IngressService) CreateIngressWithUrl(ctx context.Context, urlStr string, req *livekit.CreateIngressRequest) (*livekit.IngressInfo, error) {
	err := EnsureIngressAdminPermission(ctx)
	if err != nil {
		return nil, twirpAuthError(err)
	}
	if s.store == nil {
		return nil, ErrIngressNotConnected
	}

	if req.InputType == livekit.IngressInput_URL_INPUT {
		if req.Url == "" {
			return nil, ingress.ErrInvalidIngress("missing URL parameter")
		}
		urlObj, err := url.Parse(req.Url)
		if err != nil {
			return nil, psrpc.NewError(psrpc.InvalidArgument, err)
		}
		if urlObj.Scheme != "http" && urlObj.Scheme != "https" && urlObj.Scheme != "srt" {
			return nil, ingress.ErrInvalidIngress(fmt.Sprintf("invalid url scheme %s", urlObj.Scheme))
		}
		// Marshall the URL again for sanitization
		urlStr = urlObj.String()
	}

	var sk string
	if req.InputType != livekit.IngressInput_URL_INPUT {
		sk = guid.New("")
	}

	info := &livekit.IngressInfo{
		IngressId:           guid.New(utils.IngressPrefix),
		Name:                req.Name,
		StreamKey:           sk,
		Url:                 urlStr,
		InputType:           req.InputType,
		Audio:               req.Audio,
		Video:               req.Video,
		EnableTranscoding:   req.EnableTranscoding,
		RoomName:            req.RoomName,
		ParticipantIdentity: req.ParticipantIdentity,
		ParticipantName:     req.ParticipantName,
		ParticipantMetadata: req.ParticipantMetadata,
		State:               &livekit.IngressState{},
		Enabled:             req.Enabled,
	}

	switch req.InputType {
	case livekit.IngressInput_RTMP_INPUT,
		livekit.IngressInput_WHIP_INPUT:
		info.Reusable = true
		if err := ingress.ValidateForSerialization(info); err != nil {
			return nil, err
		}
	case livekit.IngressInput_URL_INPUT:
		if err := ingress.Validate(info); err != nil {
			return nil, err
		}
	default:
		return nil, ingress.ErrInvalidIngressType
	}

	updateEnableTranscoding(info)

	if req.InputType == livekit.IngressInput_URL_INPUT {
		retInfo, err := s.launcher.LaunchPullIngress(ctx, info)
		if retInfo != nil {
			info = retInfo
		} else {
			info.State.Status = livekit.IngressState_ENDPOINT_ERROR
			info.State.Error = err.Error()
		}
		if err != nil {
			return info, err
		}
		// The Ingress instance will create the ingress object when handling the URL pull ingress
	} else {
		_, err = s.io.CreateIngress(ctx, info)
		switch err {
		case nil:
			break
		case ingress.ErrIngressOutOfDate:
			// Error returned if the ingress was already created by the ingress service
			err = nil
		default:
			logger.Errorw("could not create ingress object", err)
			return nil, err
		}
	}

	return info, nil
}

func (s *IngressService) LaunchPullIngress(ctx context.Context, info *livekit.IngressInfo) (*livekit.IngressInfo, error) {
	req := &rpc.StartIngressRequest{
		Info: info,
	}

	return s.psrpcClient.StartIngress(ctx, req)
}

func updateEnableTranscoding(info *livekit.IngressInfo) {
	// Set BypassTranscoding as well for backward compatiblity
	if info.EnableTranscoding != nil {
		info.BypassTranscoding = !*info.EnableTranscoding
		return
	}

	switch info.InputType {
	case livekit.IngressInput_WHIP_INPUT:
		f := false
		info.EnableTranscoding = &f
		info.BypassTranscoding = true
	default:
		t := true
		info.EnableTranscoding = &t
	}
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
	if req.EnableTranscoding != nil {
		info.EnableTranscoding = req.EnableTranscoding
	}

	if req.ParticipantMetadata != "" {
		info.ParticipantMetadata = req.ParticipantMetadata
	}
	if req.Audio != nil {
		info.Audio = req.Audio
	}
	if req.Video != nil {
		info.Video = req.Video
	}

	if req.Enabled != nil {
		info.Enabled = req.Enabled
	}

	if err := ingress.ValidateForSerialization(info); err != nil {
		return err
	}

	updateEnableTranscoding(info)

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

	if !info.Reusable {
		logger.Infow("ingress update attempted on non reusable ingress", "ingressID", info.IngressId)
		return info, ErrIngressNonReusable
	}

	switch info.State.Status {
	case livekit.IngressState_ENDPOINT_ERROR:
		info.State.Status = livekit.IngressState_ENDPOINT_INACTIVE
		_, err = s.io.UpdateIngressState(ctx, &rpc.UpdateIngressStateRequest{
			IngressId: req.IngressId,
			State:     info.State,
		})
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

	var infos []*livekit.IngressInfo
	if req.IngressId != "" {
		info, err := s.store.LoadIngress(ctx, req.IngressId)
		if err != nil {
			return nil, err
		}
		infos = []*livekit.IngressInfo{info}
	} else {
		infos, err = s.store.ListIngress(ctx, livekit.RoomName(req.RoomName))
		if err != nil {
			logger.Errorw("could not list ingress info", err)
			return nil, err
		}
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

	s.telemetry.IngressDeleted(ctx, info)

	return info, nil
}
