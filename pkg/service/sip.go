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

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/protocol/utils"
	"github.com/livekit/psrpc"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/telemetry"
)

type SIPService struct {
	conf        *config.SIPConfig
	nodeID      livekit.NodeID
	bus         psrpc.MessageBus
	psrpcClient rpc.SIPClient
	store       SIPStore
	roomService livekit.RoomService
}

func NewSIPService(
	conf *config.SIPConfig,
	nodeID livekit.NodeID,
	bus psrpc.MessageBus,
	psrpcClient rpc.SIPClient,
	store SIPStore,
	rs livekit.RoomService,
	ts telemetry.TelemetryService,
) *SIPService {
	return &SIPService{
		conf:        conf,
		nodeID:      nodeID,
		bus:         bus,
		psrpcClient: psrpcClient,
		store:       store,
		roomService: rs,
	}
}

func (s *SIPService) CreateSIPTrunk(ctx context.Context, req *livekit.CreateSIPTrunkRequest) (*livekit.SIPTrunkInfo, error) {
	if s.store == nil {
		return nil, ErrSIPNotConnected
	}

	info := &livekit.SIPTrunkInfo{
		SipTrunkId:          utils.NewGuid(utils.SIPTrunkPrefix),
		InboundAddresses:    req.InboundAddresses,
		OutboundAddress:     req.OutboundAddress,
		OutboundNumber:      req.OutboundNumber,
		InboundNumbersRegex: req.InboundNumbersRegex,
		InboundUsername:     req.InboundUsername,
		InboundPassword:     req.InboundPassword,
		OutboundUsername:    req.OutboundUsername,
		OutboundPassword:    req.OutboundPassword,
	}

	if err := s.store.StoreSIPTrunk(ctx, info); err != nil {
		return nil, err
	}
	return info, nil
}

func (s *SIPService) ListSIPTrunk(ctx context.Context, req *livekit.ListSIPTrunkRequest) (*livekit.ListSIPTrunkResponse, error) {
	if s.store == nil {
		return nil, ErrSIPNotConnected
	}

	trunks, err := s.store.ListSIPTrunk(ctx)
	if err != nil {
		return nil, err
	}

	return &livekit.ListSIPTrunkResponse{Items: trunks}, nil
}

func (s *SIPService) DeleteSIPTrunk(ctx context.Context, req *livekit.DeleteSIPTrunkRequest) (*livekit.SIPTrunkInfo, error) {
	if s.store == nil {
		return nil, ErrSIPNotConnected
	}

	info, err := s.store.LoadSIPTrunk(ctx, req.SipTrunkId)
	if err != nil {
		return nil, err
	}

	if err = s.store.DeleteSIPTrunk(ctx, info); err != nil {
		return nil, err
	}

	return info, nil
}

func (s *SIPService) CreateSIPDispatchRule(ctx context.Context, req *livekit.CreateSIPDispatchRuleRequest) (*livekit.SIPDispatchRuleInfo, error) {
	if s.store == nil {
		return nil, ErrSIPNotConnected
	}

	info := &livekit.SIPDispatchRuleInfo{
		SipDispatchRuleId: utils.NewGuid(utils.SIPDispatchRulePrefix),
		Rule:              req.Rule,
		TrunkIds:          req.TrunkIds,
		HidePhoneNumber:   req.HidePhoneNumber,
	}

	if err := s.store.StoreSIPDispatchRule(ctx, info); err != nil {
		return nil, err
	}
	return info, nil
}

func (s *SIPService) ListSIPDispatchRule(ctx context.Context, req *livekit.ListSIPDispatchRuleRequest) (*livekit.ListSIPDispatchRuleResponse, error) {
	if s.store == nil {
		return nil, ErrSIPNotConnected
	}

	rules, err := s.store.ListSIPDispatchRule(ctx)
	if err != nil {
		return nil, err
	}

	return &livekit.ListSIPDispatchRuleResponse{Items: rules}, nil
}

func (s *SIPService) DeleteSIPDispatchRule(ctx context.Context, req *livekit.DeleteSIPDispatchRuleRequest) (*livekit.SIPDispatchRuleInfo, error) {
	if s.store == nil {
		return nil, ErrSIPNotConnected
	}

	info, err := s.store.LoadSIPDispatchRule(ctx, req.SipDispatchRuleId)
	if err != nil {
		return nil, err
	}

	if err = s.store.DeleteSIPDispatchRule(ctx, info); err != nil {
		return nil, err
	}

	return info, nil
}

func (s *SIPService) CreateSIPParticipant(ctx context.Context, req *livekit.CreateSIPParticipantRequest) (*livekit.SIPParticipantInfo, error) {
	if s.store == nil {
		return nil, ErrSIPNotConnected
	}

	info := &livekit.SIPParticipantInfo{
		SipParticipantId:    utils.NewGuid(utils.SIPParticipantPrefix),
		SipTrunkId:          req.SipTrunkId,
		SipCallTo:           req.SipCallTo,
		RoomName:            req.RoomName,
		ParticipantIdentity: req.ParticipantIdentity,
	}

	if err := s.store.StoreSIPParticipant(ctx, info); err != nil {
		return nil, err
	}
	s.updateParticipant(ctx, info)
	return info, nil
}

func (s *SIPService) updateParticipant(ctx context.Context, info *livekit.SIPParticipantInfo) {
	AppendLogFields(ctx, "participantId", info.SipParticipantId, "room", info.RoomName, "trunk", info.SipTrunkId, "to", info.SipCallTo)
	req := &rpc.InternalUpdateSIPParticipantRequest{
		ParticipantId:       info.SipParticipantId,
		CallTo:              info.SipCallTo,
		RoomName:            info.RoomName,
		ParticipantIdentity: info.ParticipantIdentity,
	}
	if info.SipTrunkId != "" {
		trunk, err := s.store.LoadSIPTrunk(ctx, info.SipTrunkId)
		if err != nil {
			logger.Errorw("cannot get trunk to update sip participant", err)
			return
		}
		req.Address = trunk.OutboundAddress
		req.Number = trunk.OutboundNumber
		req.Username = trunk.OutboundUsername
		req.Password = trunk.OutboundPassword
	}
	if _, err := s.psrpcClient.UpdateSIPParticipant(ctx, req); err != nil {
		logger.Errorw("cannot update sip participant", err)
	}
}

func (s *SIPService) ListSIPParticipant(ctx context.Context, req *livekit.ListSIPParticipantRequest) (*livekit.ListSIPParticipantResponse, error) {
	if s.store == nil {
		return nil, ErrSIPNotConnected
	}

	participants, err := s.store.ListSIPParticipant(ctx)
	if err != nil {
		return nil, err
	}

	return &livekit.ListSIPParticipantResponse{Items: participants}, nil
}

func (s *SIPService) DeleteSIPParticipant(ctx context.Context, req *livekit.DeleteSIPParticipantRequest) (*livekit.SIPParticipantInfo, error) {
	if s.store == nil {
		return nil, ErrSIPNotConnected
	}

	info, err := s.store.LoadSIPParticipant(ctx, req.SipParticipantId)
	if err != nil {
		return nil, err
	}

	if err = s.store.DeleteSIPParticipant(ctx, info); err != nil {
		return nil, err
	}
	// These indicate that the call should be disconnected
	info.SipTrunkId = ""
	info.SipCallTo = ""
	s.updateParticipant(ctx, info)
	return info, nil
}

func (s *SIPService) SendSIPParticipantDTMF(ctx context.Context, req *livekit.SendSIPParticipantDTMFRequest) (*livekit.SIPParticipantDTMFInfo, error) {
	if s.store == nil {
		return nil, ErrSIPNotConnected
	}
	AppendLogFields(ctx, "participantId", req.SipParticipantId)
	_, err := s.psrpcClient.SendSIPParticipantDTMF(ctx, &rpc.InternalSendSIPParticipantDTMFRequest{
		ParticipantId: req.SipParticipantId,
		Digits:        req.Digits,
	})
	if err != nil {
		logger.Errorw("cannot send dtmf to sip participant", err)
	}
	return nil, err
}
