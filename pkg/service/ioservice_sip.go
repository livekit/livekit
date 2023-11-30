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
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/protocol/sip"
)

// matchSIPTrunk finds a SIP Trunk definition matching the request.
// Returns nil if no rules matched or an error if there are conflicting definitions.
func (s *IOInfoService) matchSIPTrunk(ctx context.Context, calling, called string) (*livekit.SIPTrunkInfo, error) {
	trunks, err := s.ss.ListSIPTrunk(ctx)
	if err != nil {
		return nil, err
	}
	return sip.MatchTrunk(trunks, calling, called)
}

// matchSIPDispatchRule finds the best dispatch rule matching the request parameters. Returns an error if no rule matched.
// Trunk parameter can be nil, in which case only wildcard dispatch rules will be effective (ones without Trunk IDs).
func (s *IOInfoService) matchSIPDispatchRule(ctx context.Context, trunk *livekit.SIPTrunkInfo, req *rpc.EvaluateSIPDispatchRulesRequest) (*livekit.SIPDispatchRuleInfo, error) {
	// Trunk can still be nil here in case none matched or were defined.
	// This is still fine, but only in case we'll match exactly one wildcard dispatch rule.
	rules, err := s.ss.ListSIPDispatchRule(ctx)
	if err != nil {
		return nil, err
	}
	return sip.MatchDispatchRule(trunk, rules, req)
}

func (s *IOInfoService) EvaluateSIPDispatchRules(ctx context.Context, req *rpc.EvaluateSIPDispatchRulesRequest) (*rpc.EvaluateSIPDispatchRulesResponse, error) {
	trunk, err := s.matchSIPTrunk(ctx, req.CallingNumber, req.CalledNumber)
	if err != nil {
		return nil, err
	}
	best, err := s.matchSIPDispatchRule(ctx, trunk, req)
	if err != nil {
		return nil, err
	}
	return sip.EvaluateDispatchRule(best, req)
}

func (s *IOInfoService) GetSIPTrunkAuthentication(ctx context.Context, req *rpc.GetSIPTrunkAuthenticationRequest) (*rpc.GetSIPTrunkAuthenticationResponse, error) {
	trunk, err := s.matchSIPTrunk(ctx, req.From, req.To)
	if err != nil {
		return nil, err
	}
	return &rpc.GetSIPTrunkAuthenticationResponse{
		Username: trunk.InboundUsername,
		Password: trunk.InboundPassword,
	}, nil
}
