// Copyright 2024 LiveKit, Inc.
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
	"slices"
	"strings"
	"testing"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/utils"
	"github.com/livekit/protocol/utils/guid"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/livekit-server/pkg/service"
)

func TestSIPStoreDispatch(t *testing.T) {
	ctx := context.Background()
	rs := redisStore(t)

	id := guid.New(utils.SIPDispatchRulePrefix)

	// No dispatch rules initially.
	list, err := rs.ListSIPDispatchRule(ctx)
	require.NoError(t, err)
	require.Empty(t, list)

	// Loading non-existent dispatch should return proper not found error.
	got, err := rs.LoadSIPDispatchRule(ctx, id)
	require.Equal(t, service.ErrSIPDispatchRuleNotFound, err)
	require.Nil(t, got)

	// Creation without ID should fail.
	rule := &livekit.SIPDispatchRuleInfo{
		TrunkIds: []string{"trunk"},
		Rule: &livekit.SIPDispatchRule{Rule: &livekit.SIPDispatchRule_DispatchRuleDirect{
			DispatchRuleDirect: &livekit.SIPDispatchRuleDirect{
				RoomName: "room",
				Pin:      "1234",
			},
		}},
	}
	err = rs.StoreSIPDispatchRule(ctx, rule)
	require.Error(t, err)

	// Creation
	rule.SipDispatchRuleId = id
	err = rs.StoreSIPDispatchRule(ctx, rule)
	require.NoError(t, err)

	// Loading
	got, err = rs.LoadSIPDispatchRule(ctx, id)
	require.NoError(t, err)
	require.True(t, proto.Equal(rule, got))

	// Listing
	list, err = rs.ListSIPDispatchRule(ctx)
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.True(t, proto.Equal(rule, list[0]))

	// Deletion. Should not return error if not exists.
	err = rs.DeleteSIPDispatchRule(ctx, &livekit.SIPDispatchRuleInfo{SipDispatchRuleId: id})
	require.NoError(t, err)
	err = rs.DeleteSIPDispatchRule(ctx, &livekit.SIPDispatchRuleInfo{SipDispatchRuleId: id})
	require.NoError(t, err)

	// Check that it's deleted.
	list, err = rs.ListSIPDispatchRule(ctx)
	require.NoError(t, err)
	require.Empty(t, list)

	got, err = rs.LoadSIPDispatchRule(ctx, id)
	require.Equal(t, service.ErrSIPDispatchRuleNotFound, err)
	require.Nil(t, got)
}

func TestSIPStoreTrunk(t *testing.T) {
	ctx := context.Background()
	rs := redisStore(t)

	oldID := guid.New(utils.SIPTrunkPrefix)
	inID := guid.New(utils.SIPTrunkPrefix)
	outID := guid.New(utils.SIPTrunkPrefix)

	// No trunks initially. Check legacy, inbound, outbound.
	// Loading non-existent trunk should return proper not found error.
	oldList, err := rs.ListSIPTrunk(ctx)
	require.NoError(t, err)
	require.Empty(t, oldList)

	old, err := rs.LoadSIPTrunk(ctx, oldID)
	require.Equal(t, service.ErrSIPTrunkNotFound, err)
	require.Nil(t, old)

	inList, err := rs.ListSIPInboundTrunk(ctx)
	require.NoError(t, err)
	require.Empty(t, inList)

	in, err := rs.LoadSIPInboundTrunk(ctx, oldID)
	require.Equal(t, service.ErrSIPTrunkNotFound, err)
	require.Nil(t, in)

	outList, err := rs.ListSIPOutboundTrunk(ctx)
	require.NoError(t, err)
	require.Empty(t, outList)

	out, err := rs.LoadSIPOutboundTrunk(ctx, oldID)
	require.Equal(t, service.ErrSIPTrunkNotFound, err)
	require.Nil(t, out)

	// Creation without ID should fail.
	oldT := &livekit.SIPTrunkInfo{
		Name: "Legacy",
	}
	err = rs.StoreSIPTrunk(ctx, oldT)
	require.Error(t, err)

	inT := &livekit.SIPInboundTrunkInfo{
		Name: "Inbound",
	}
	err = rs.StoreSIPInboundTrunk(ctx, inT)
	require.Error(t, err)

	outT := &livekit.SIPOutboundTrunkInfo{
		Name: "Outbound",
	}
	err = rs.StoreSIPOutboundTrunk(ctx, outT)
	require.Error(t, err)

	// Creation
	oldT.SipTrunkId = oldID
	err = rs.StoreSIPTrunk(ctx, oldT)
	require.NoError(t, err)

	inT.SipTrunkId = inID
	err = rs.StoreSIPInboundTrunk(ctx, inT)
	require.NoError(t, err)

	outT.SipTrunkId = outID
	err = rs.StoreSIPOutboundTrunk(ctx, outT)
	require.NoError(t, err)

	// Loading (with matching kind)
	oldT2, err := rs.LoadSIPTrunk(ctx, oldID)
	require.NoError(t, err)
	require.True(t, proto.Equal(oldT, oldT2))

	inT2, err := rs.LoadSIPInboundTrunk(ctx, inID)
	require.NoError(t, err)
	require.True(t, proto.Equal(inT, inT2))

	outT2, err := rs.LoadSIPOutboundTrunk(ctx, outID)
	require.NoError(t, err)
	require.True(t, proto.Equal(outT, outT2))

	// Loading (compat)
	oldT2, err = rs.LoadSIPTrunk(ctx, inID)
	require.NoError(t, err)
	require.True(t, proto.Equal(inT.AsTrunkInfo(), oldT2))

	oldT2, err = rs.LoadSIPTrunk(ctx, outID)
	require.NoError(t, err)
	require.True(t, proto.Equal(outT.AsTrunkInfo(), oldT2))

	inT2, err = rs.LoadSIPInboundTrunk(ctx, oldID)
	require.NoError(t, err)
	require.True(t, proto.Equal(oldT.AsInbound(), inT2))

	outT2, err = rs.LoadSIPOutboundTrunk(ctx, oldID)
	require.NoError(t, err)
	require.True(t, proto.Equal(oldT.AsOutbound(), outT2))

	// Listing (always shows legacy + new)
	listOld, err := rs.ListSIPTrunk(ctx)
	require.NoError(t, err)
	require.Len(t, listOld, 3)
	slices.SortFunc(listOld, func(a, b *livekit.SIPTrunkInfo) int {
		return strings.Compare(a.Name, b.Name)
	})
	require.True(t, proto.Equal(inT.AsTrunkInfo(), listOld[0]))
	require.True(t, proto.Equal(oldT, listOld[1]))
	require.True(t, proto.Equal(outT.AsTrunkInfo(), listOld[2]))

	listIn, err := rs.ListSIPInboundTrunk(ctx)
	require.NoError(t, err)
	require.Len(t, listIn, 2)
	slices.SortFunc(listIn, func(a, b *livekit.SIPInboundTrunkInfo) int {
		return strings.Compare(a.Name, b.Name)
	})
	require.True(t, proto.Equal(inT, listIn[0]))
	require.True(t, proto.Equal(oldT.AsInbound(), listIn[1]))

	listOut, err := rs.ListSIPOutboundTrunk(ctx)
	require.NoError(t, err)
	require.Len(t, listOut, 2)
	slices.SortFunc(listOut, func(a, b *livekit.SIPOutboundTrunkInfo) int {
		return strings.Compare(a.Name, b.Name)
	})
	require.True(t, proto.Equal(oldT.AsOutbound(), listOut[0]))
	require.True(t, proto.Equal(outT, listOut[1]))

	// Deletion. Should not return error if not exists.
	err = rs.DeleteSIPTrunk(ctx, &livekit.SIPTrunkInfo{SipTrunkId: oldID})
	require.NoError(t, err)
	err = rs.DeleteSIPTrunk(ctx, &livekit.SIPTrunkInfo{SipTrunkId: oldID})
	require.NoError(t, err)

	// Other objects are still there.
	inT2, err = rs.LoadSIPInboundTrunk(ctx, inID)
	require.NoError(t, err)
	require.True(t, proto.Equal(inT, inT2))

	outT2, err = rs.LoadSIPOutboundTrunk(ctx, outID)
	require.NoError(t, err)
	require.True(t, proto.Equal(outT, outT2))

	// Delete the rest
	err = rs.DeleteSIPTrunk(ctx, &livekit.SIPTrunkInfo{SipTrunkId: inID})
	require.NoError(t, err)
	err = rs.DeleteSIPTrunk(ctx, &livekit.SIPTrunkInfo{SipTrunkId: outID})
	require.NoError(t, err)

	// Check everything is deleted.
	oldList, err = rs.ListSIPTrunk(ctx)
	require.NoError(t, err)
	require.Empty(t, oldList)

	inList, err = rs.ListSIPInboundTrunk(ctx)
	require.NoError(t, err)
	require.Empty(t, inList)

	outList, err = rs.ListSIPOutboundTrunk(ctx)
	require.NoError(t, err)
	require.Empty(t, outList)

	old, err = rs.LoadSIPTrunk(ctx, oldID)
	require.Equal(t, service.ErrSIPTrunkNotFound, err)
	require.Nil(t, old)

	in, err = rs.LoadSIPInboundTrunk(ctx, oldID)
	require.Equal(t, service.ErrSIPTrunkNotFound, err)
	require.Nil(t, in)

	out, err = rs.LoadSIPOutboundTrunk(ctx, oldID)
	require.Equal(t, service.ErrSIPTrunkNotFound, err)
	require.Nil(t, out)
}
