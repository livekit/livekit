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
	"errors"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/protocol/sip"
	"github.com/livekit/psrpc"
	"google.golang.org/protobuf/types/known/emptypb"
)

// matchSIPTrunk finds a SIP Trunk definition matching the request.
// Returns nil if no rules matched or an error if there are conflicting definitions.
func (s *IOInfoService) matchSIPTrunk(ctx context.Context, trunkID, calling, called string) (*livekit.SIPInboundTrunkInfo, error) {
	trunks, err := s.ss.ListSIPInboundTrunk(ctx)
	if err != nil {
		return nil, err
	}
	return sip.MatchTrunk(trunks, "", calling, called)
}

// matchSIPDispatchRule finds the best dispatch rule matching the request parameters. Returns an error if no rule matched.
// Trunk parameter can be nil, in which case only wildcard dispatch rules will be effective (ones without Trunk IDs).
func (s *IOInfoService) matchSIPDispatchRule(ctx context.Context, trunk *livekit.SIPInboundTrunkInfo, req *rpc.EvaluateSIPDispatchRulesRequest) (*livekit.SIPDispatchRuleInfo, error) {
	// Trunk can still be nil here in case none matched or were defined.
	// This is still fine, but only in case we'll match exactly one wildcard dispatch rule.
	rules, err := s.ss.ListSIPDispatchRule(ctx)
	if err != nil {
		return nil, err
	}
	return sip.MatchDispatchRule(trunk, rules, req)
}

func (s *IOInfoService) EvaluateSIPDispatchRules(ctx context.Context, req *rpc.EvaluateSIPDispatchRulesRequest) (*rpc.EvaluateSIPDispatchRulesResponse, error) {
	log := logger.GetLogger()
	log = log.WithValues("toUser", req.CalledNumber, "fromUser", req.CallingNumber)
	trunk, err := s.matchSIPTrunk(ctx, req.SipTrunkId, req.CallingNumber, req.CalledNumber)
	if err != nil {
		return nil, err
	}
	trunkID := ""
	if trunk != nil {
		trunkID = trunk.SipTrunkId
	}
	log = log.WithValues("sipTrunk", trunkID)
	if trunk != nil {
		log.Debugw("SIP trunk matched")
	} else {
		log.Debugw("No SIP trunk matched")
	}
	best, err := s.matchSIPDispatchRule(ctx, trunk, req)
	if err != nil {
		if e := (*sip.ErrNoDispatchMatched)(nil); errors.As(err, &e) {
			return &rpc.EvaluateSIPDispatchRulesResponse{
				SipTrunkId: trunkID,
				Result:     rpc.SIPDispatchResult_DROP,
			}, nil
		}
		return nil, err
	}
	log.Debugw("SIP dispatch rule matched", "sipRule", best.SipDispatchRuleId)
	resp, err := sip.EvaluateDispatchRule("", trunk, best, req)
	if err != nil {
		return nil, err
	}
	resp.SipTrunkId = trunkID
	return resp, err
}

func (s *IOInfoService) GetSIPTrunkAuthentication(ctx context.Context, req *rpc.GetSIPTrunkAuthenticationRequest) (*rpc.GetSIPTrunkAuthenticationResponse, error) {
	log := logger.GetLogger()
	log = log.WithValues("toUser", req.To, "fromUser", req.From)
	trunk, err := s.matchSIPTrunk(ctx, "", req.From, req.To)
	if err != nil {
		return nil, err
	}
	if trunk == nil {
		log.Debugw("No SIP trunk matched for auth", "sipTrunk", "")
		return &rpc.GetSIPTrunkAuthenticationResponse{}, nil
	}
	log.Debugw("SIP trunk matched for auth", "sipTrunk", trunk.SipTrunkId)
	return &rpc.GetSIPTrunkAuthenticationResponse{
		SipTrunkId: trunk.SipTrunkId,
		Username:   trunk.AuthUsername,
		Password:   trunk.AuthPassword,
	}, nil
}

func (s *IOInfoService) UpdateSIPCallState(ctx context.Context, req *rpc.UpdateSIPCallStateRequest) (*emptypb.Empty, error) {
	log := logger.GetLogger()
	log = log.WithValues("callID", req.CallInfo.CallId, "callStatus", req.CallInfo.CallStatus.String())

	log.Infow("SIP call state updated")

	if req.CallInfo == nil {
		return nil, psrpc.NewErrorf(psrpc.InvalidArgument, "no call info in UpdateSIPCallState request")
	}

	switch req.CallInfo.CallStatus {
	case livekit.SIPCallStatus_SIP_CALL_STATUS_PARTICIPANT_JOINED:
		s.telemetry.SIPParticipantCreated(ctx, req.CallInfo)
	case livekit.SIPCallStatus_SIP_CALL_STATUS_CALL_INCOMING:
		s.telemetry.SIPCallIncoming(ctx, req.CallInfo)
	case livekit.SIPCallStatus_SIP_CALL_STATUS_ACTIVE:
		s.telemetry.SIPCallStarted(ctx, req.CallInfo)
	case livekit.SIPCallStatus_SIP_CALL_STATUS_DISCONNECTED:
		s.telemetry.SIPCallEnded(ctx, req.CallInfo)
	}

	return &emptypb.Empty{}, nil
}
