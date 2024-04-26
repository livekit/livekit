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
	"time"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/protocol/sip"
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
	if len(req.InboundNumbersRegex) != 0 {
		return nil, fmt.Errorf("Trunks with InboundNumbersRegex are deprecated. Use InboundNumbers instead.")
	}

	// Keep ID empty, so that validation can print "<new>" instead of a non-existent ID in the error.
	info := &livekit.SIPTrunkInfo{
		InboundAddresses: req.InboundAddresses,
		OutboundAddress:  req.OutboundAddress,
		OutboundNumber:   req.OutboundNumber,
		InboundNumbers:   req.InboundNumbers,
		InboundUsername:  req.InboundUsername,
		InboundPassword:  req.InboundPassword,
		OutboundUsername: req.OutboundUsername,
		OutboundPassword: req.OutboundPassword,
	}

	// Validate all trunks including the new one first.
	list, err := s.store.ListSIPTrunk(ctx)
	if err != nil {
		return nil, err
	}
	list = append(list, info)
	if err = sip.ValidateTrunks(list); err != nil {
		return nil, err
	}

	// Now we can generate ID and store.
	info.SipTrunkId = utils.NewGuid(utils.SIPTrunkPrefix)
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

	// Keep ID empty, so that validation can print "<new>" instead of a non-existent ID in the error.
	info := &livekit.SIPDispatchRuleInfo{
		Rule:            req.Rule,
		TrunkIds:        req.TrunkIds,
		HidePhoneNumber: req.HidePhoneNumber,
	}

	// Validate all rules including the new one first.
	list, err := s.store.ListSIPDispatchRule(ctx)
	if err != nil {
		return nil, err
	}
	list = append(list, info)
	if err = sip.ValidateDispatchRules(list); err != nil {
		return nil, err
	}

	// Now we can generate ID and store.
	info.SipDispatchRuleId = utils.NewGuid(utils.SIPDispatchRulePrefix)
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

func (s *SIPService) CreateSIPParticipantWithToken(ctx context.Context, req *livekit.CreateSIPParticipantRequest, wsUrl, token string) (*livekit.SIPParticipantInfo, error) {
	if s.store == nil {
		return nil, ErrSIPNotConnected
	}

	AppendLogFields(ctx, "room", req.RoomName, "trunk", req.SipTrunkId, "to", req.SipCallTo)
	ireq := &rpc.InternalCreateSIPParticipantRequest{
		CallTo:              req.SipCallTo,
		RoomName:            req.RoomName,
		ParticipantIdentity: req.ParticipantIdentity,
		Dtmf:                req.Dtmf,
		PlayRingtone:        req.PlayRingtone,
		WsUrl:               wsUrl,
		Token:               token,
	}
	if req.SipTrunkId != "" {
		trunk, err := s.store.LoadSIPTrunk(ctx, req.SipTrunkId)
		if err != nil {
			logger.Errorw("cannot get trunk to update sip participant", err)
			return nil, err
		}
		ireq.Address = trunk.OutboundAddress
		ireq.Number = trunk.OutboundNumber
		ireq.Username = trunk.OutboundUsername
		ireq.Password = trunk.OutboundPassword
	}

	// CreateSIPParticipant will wait for LiveKit Participant to be created and that can take some time.
	// Thus, we must set a higher deadline for it, if it's not set already.
	// TODO: support context timeouts in psrpc
	timeout := 30 * time.Second
	if deadline, ok := ctx.Deadline(); ok {
		timeout = time.Until(deadline)
	} else {
		var cancel func()
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	resp, err := s.psrpcClient.CreateSIPParticipant(ctx, "", ireq, psrpc.WithRequestTimeout(timeout))
	if err != nil {
		logger.Errorw("cannot update sip participant", err)
		return nil, err
	}
	return &livekit.SIPParticipantInfo{
		ParticipantId:       resp.ParticipantId,
		ParticipantIdentity: resp.ParticipantIdentity,
		RoomName:            req.RoomName,
	}, nil
}
func (s *SIPService) CreateSIPParticipant(ctx context.Context, req *livekit.CreateSIPParticipantRequest) (*livekit.SIPParticipantInfo, error) {
	return s.CreateSIPParticipantWithToken(ctx, req, "", "")
}
