// Copyright 2026 LiveKit, Inc.
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

package service_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/twitchtv/twirp"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"

	"github.com/livekit/livekit-server/pkg/service"
	"github.com/livekit/livekit-server/pkg/service/servicefakes"
)

func newTestSIPService(store service.SIPStore) (*service.SIPService, context.Context) {
	ctx := service.WithGrants(context.Background(), &auth.ClaimGrants{SIP: &auth.SIPGrant{Admin: true}}, "")
	return service.NewSIPService(nil, "", nil, nil, store, nil, nil), ctx
}

func requireInvalidArgument(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	var terr twirp.Error
	require.ErrorAs(t, err, &terr)
	require.Equal(t, twirp.InvalidArgument, terr.Code())
}

func TestUpdateSIPInboundTrunkDetectsConflictOnNewNumbers(t *testing.T) {
	existing := &livekit.SIPInboundTrunkInfo{SipTrunkId: "ST_existing", Numbers: []string{"+15550002222"}}
	updated := &livekit.SIPInboundTrunkInfo{SipTrunkId: "ST_updated", Numbers: []string{"+15550001111"}}

	store := &servicefakes.FakeSIPStore{}
	store.LoadSIPInboundTrunkReturns(updated, nil)
	store.ListSIPInboundTrunkReturns(&livekit.ListSIPInboundTrunkResponse{
		Items: []*livekit.SIPInboundTrunkInfo{existing, updated},
	}, nil)
	s, ctx := newTestSIPService(store)

	// Moving the trunk to a number that another trunk already uses, without
	// allowed numbers on either trunk, is the same conflict that creation rejects.
	_, err := s.UpdateSIPInboundTrunk(ctx, &livekit.UpdateSIPInboundTrunkRequest{
		SipTrunkId: updated.SipTrunkId,
		Action: &livekit.UpdateSIPInboundTrunkRequest_Update{Update: &livekit.SIPInboundTrunkUpdate{
			Numbers: &livekit.ListUpdate{Set: []string{"+15550002222"}},
		}},
	})
	requireInvalidArgument(t, err)
	require.Zero(t, store.StoreSIPInboundTrunkCallCount())
}

func TestUpdateSIPDispatchRuleDetectsConflictOnNewTrunks(t *testing.T) {
	direct := func(room string) *livekit.SIPDispatchRule {
		return &livekit.SIPDispatchRule{Rule: &livekit.SIPDispatchRule_DispatchRuleDirect{
			DispatchRuleDirect: &livekit.SIPDispatchRuleDirect{RoomName: room},
		}}
	}
	existing := &livekit.SIPDispatchRuleInfo{SipDispatchRuleId: "SDR_existing", TrunkIds: []string{"ST_2"}, Rule: direct("a")}
	updated := &livekit.SIPDispatchRuleInfo{SipDispatchRuleId: "SDR_updated", TrunkIds: []string{"ST_1"}, Rule: direct("b")}

	store := &servicefakes.FakeSIPStore{}
	store.LoadSIPDispatchRuleReturns(updated, nil)
	store.ListSIPDispatchRuleReturns(&livekit.ListSIPDispatchRuleResponse{
		Items: []*livekit.SIPDispatchRuleInfo{existing, updated},
	}, nil)
	s, ctx := newTestSIPService(store)

	// Moving the rule to a trunk that already has a rule for the same numbers and PIN
	// is the same conflict that creation rejects.
	_, err := s.UpdateSIPDispatchRule(ctx, &livekit.UpdateSIPDispatchRuleRequest{
		SipDispatchRuleId: updated.SipDispatchRuleId,
		Action: &livekit.UpdateSIPDispatchRuleRequest_Update{Update: &livekit.SIPDispatchRuleUpdate{
			TrunkIds: &livekit.ListUpdate{Set: []string{"ST_2"}},
		}},
	})
	requireInvalidArgument(t, err)
	require.Zero(t, store.StoreSIPDispatchRuleCallCount())
}

func TestUpdateSIPDispatchRuleKeepsUnchangedTrunks(t *testing.T) {
	rule := &livekit.SIPDispatchRuleInfo{
		SipDispatchRuleId: "SDR_rule",
		TrunkIds:          []string{"ST_1"},
		Rule: &livekit.SIPDispatchRule{Rule: &livekit.SIPDispatchRule_DispatchRuleDirect{
			DispatchRuleDirect: &livekit.SIPDispatchRuleDirect{RoomName: "a"},
		}},
	}

	store := &servicefakes.FakeSIPStore{}
	store.LoadSIPDispatchRuleReturns(rule, nil)
	store.ListSIPDispatchRuleReturns(&livekit.ListSIPDispatchRuleResponse{
		Items: []*livekit.SIPDispatchRuleInfo{rule},
	}, nil)
	s, ctx := newTestSIPService(store)

	// The stored copy of the rule must not conflict with its updated version.
	name := "renamed"
	got, err := s.UpdateSIPDispatchRule(ctx, &livekit.UpdateSIPDispatchRuleRequest{
		SipDispatchRuleId: rule.SipDispatchRuleId,
		Action: &livekit.UpdateSIPDispatchRuleRequest_Update{Update: &livekit.SIPDispatchRuleUpdate{
			Name:     &name,
			TrunkIds: &livekit.ListUpdate{Add: []string{"ST_2"}},
		}},
	})
	require.NoError(t, err)
	require.Equal(t, name, got.Name)
	require.Equal(t, []string{"ST_1", "ST_2"}, got.TrunkIds)
	require.Equal(t, 1, store.StoreSIPDispatchRuleCallCount())
}
