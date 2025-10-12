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
	"net/netip"

	"github.com/dennwc/iters"
	"github.com/twitchtv/twirp"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/protocol/sip"
)

// matchSIPTrunk finds a SIP Trunk definition matching the request.
// Returns nil if no rules matched or an error if there are conflicting definitions.
func (s *IOInfoService) matchSIPTrunk(ctx context.Context, trunkID string, call *rpc.SIPCall) (*livekit.SIPInboundTrunkInfo, error) {
	if s.ss == nil {
		return nil, ErrSIPNotConnected
	}
	if trunkID != "" {
		// This is a best-effort optimization. Fallthrough to listing trunks if it doesn't work.
		if tr, err := s.ss.LoadSIPInboundTrunk(ctx, trunkID); err == nil {
			tr, err = sip.MatchTrunkIter(iters.Slice([]*livekit.SIPInboundTrunkInfo{tr}), call)
			if err == nil {
				return tr, nil
			}
		}
	}
	it := s.SelectSIPInboundTrunk(ctx, call.To.User)
	return sip.MatchTrunkIter(it, call)
}

func (s *IOInfoService) SelectSIPInboundTrunk(ctx context.Context, called string) iters.Iter[*livekit.SIPInboundTrunkInfo] {
	it := livekit.ListPageIter(s.ss.ListSIPInboundTrunk, &livekit.ListSIPInboundTrunkRequest{
		Numbers: []string{called},
	})
	return iters.PagesAsIter(ctx, it)
}

// matchSIPDispatchRule finds the best dispatch rule matching the request parameters. Returns an error if no rule matched.
// Trunk parameter can be nil, in which case only wildcard dispatch rules will be effective (ones without Trunk IDs).
func (s *IOInfoService) matchSIPDispatchRule(ctx context.Context, trunk *livekit.SIPInboundTrunkInfo, req *rpc.EvaluateSIPDispatchRulesRequest) (*livekit.SIPDispatchRuleInfo, error) {
	if s.ss == nil {
		return nil, ErrSIPNotConnected
	}
	var trunkID string
	if trunk != nil {
		trunkID = trunk.SipTrunkId
	}
	// Trunk can still be nil here in case none matched or were defined.
	// This is still fine, but only in case we'll match exactly one wildcard dispatch rule.
	it := s.SelectSIPDispatchRule(ctx, trunkID)
	return sip.MatchDispatchRuleIter(trunk, it, req)
}

func (s *IOInfoService) SelectSIPDispatchRule(ctx context.Context, trunkID string) iters.Iter[*livekit.SIPDispatchRuleInfo] {
	var trunkIDs []string
	if trunkID != "" {
		trunkIDs = []string{trunkID}
	}
	it := livekit.ListPageIter(s.ss.ListSIPDispatchRule, &livekit.ListSIPDispatchRuleRequest{
		TrunkIds: trunkIDs,
	})
	return iters.PagesAsIter(ctx, it)
}

func (s *IOInfoService) EvaluateSIPDispatchRules(ctx context.Context, req *rpc.EvaluateSIPDispatchRulesRequest) (*rpc.EvaluateSIPDispatchRulesResponse, error) {
	call := req.SIPCall()
	log := logger.GetLogger()
	log = log.WithValues("toUser", call.To.User, "fromUser", call.From.User, "src", call.SourceIp)
	if call.SourceIp == "" {
		log.Warnw("source address is not set", nil)
		// TODO: return error in the next release
	}
	_, err := netip.ParseAddr(call.SourceIp)
	if call.SourceIp != "" && err != nil {
		log.Errorw("cannot parse source IP", err)
		return nil, twirp.WrapError(twirp.NewError(twirp.InvalidArgument, err.Error()), err)
	}
	trunk, err := s.matchSIPTrunk(ctx, req.SipTrunkId, call)
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
	call := req.SIPCall()
	log := logger.GetLogger()
	log = log.WithValues("toUser", call.To.User, "fromUser", call.From.User, "src", call.SourceIp)
	if call.SourceIp == "" {
		log.Warnw("source address is not set", nil)
		// TODO: return error in the next release
	}
	_, err := netip.ParseAddr(call.SourceIp)
	if call.SourceIp != "" && err != nil {
		log.Errorw("cannot parse source IP", err)
		return nil, twirp.WrapError(twirp.NewError(twirp.InvalidArgument, err.Error()), err)
	}
	trunk, err := s.matchSIPTrunk(ctx, "", call)
	if err != nil {
		return nil, err
	}
	if trunk == nil {
		log.Debugw("No SIP trunk matched for auth", "sipTrunk", "")
		return &rpc.GetSIPTrunkAuthenticationResponse{}, nil
	}
	log.Debugw("SIP trunk matched for auth", "sipTrunk", trunk.SipTrunkId)

	// Create provider info for the trunk
	providerInfo := &livekit.ProviderInfo{
		Id:   trunk.SipTrunkId,
		Name: trunk.Name,
		Type: livekit.ProviderType_PROVIDER_TYPE_EXTERNAL, // External trunk
	}

	return &rpc.GetSIPTrunkAuthenticationResponse{
		SipTrunkId:   trunk.SipTrunkId,
		Username:     trunk.AuthUsername,
		Password:     trunk.AuthPassword,
		ProviderInfo: providerInfo,
	}, nil
}
