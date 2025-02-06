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
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/dennwc/iters"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/utils"
	"github.com/livekit/protocol/utils/guid"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/livekit-server/pkg/service"
)

func TestSIPStoreDispatch(t *testing.T) {
	ctx := context.Background()
	rs := redisStoreDocker(t)

	id := guid.New(utils.SIPDispatchRulePrefix)

	// No dispatch rules initially.
	list, err := rs.ListSIPDispatchRule(ctx, &livekit.ListSIPDispatchRuleRequest{})
	require.NoError(t, err)
	require.Empty(t, list.Items)

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
	list, err = rs.ListSIPDispatchRule(ctx, &livekit.ListSIPDispatchRuleRequest{})
	require.NoError(t, err)
	require.Len(t, list.Items, 1)
	require.True(t, proto.Equal(rule, list.Items[0]))

	// Deletion. Should not return error if not exists.
	err = rs.DeleteSIPDispatchRule(ctx, id)
	require.NoError(t, err)
	err = rs.DeleteSIPDispatchRule(ctx, id)
	require.NoError(t, err)

	// Check that it's deleted.
	list, err = rs.ListSIPDispatchRule(ctx, &livekit.ListSIPDispatchRuleRequest{})
	require.NoError(t, err)
	require.Empty(t, list.Items)

	got, err = rs.LoadSIPDispatchRule(ctx, id)
	require.Equal(t, service.ErrSIPDispatchRuleNotFound, err)
	require.Nil(t, got)
}

func TestSIPStoreTrunk(t *testing.T) {
	ctx := context.Background()
	rs := redisStoreDocker(t)

	oldID := guid.New(utils.SIPTrunkPrefix)
	inID := guid.New(utils.SIPTrunkPrefix)
	outID := guid.New(utils.SIPTrunkPrefix)

	// No trunks initially. Check legacy, inbound, outbound.
	// Loading non-existent trunk should return proper not found error.
	oldList, err := rs.ListSIPTrunk(ctx, &livekit.ListSIPTrunkRequest{})
	require.NoError(t, err)
	require.Empty(t, oldList.Items)

	old, err := rs.LoadSIPTrunk(ctx, oldID)
	require.Equal(t, service.ErrSIPTrunkNotFound, err)
	require.Nil(t, old)

	inList, err := rs.ListSIPInboundTrunk(ctx, &livekit.ListSIPInboundTrunkRequest{})
	require.NoError(t, err)
	require.Empty(t, inList.Items)

	in, err := rs.LoadSIPInboundTrunk(ctx, oldID)
	require.Equal(t, service.ErrSIPTrunkNotFound, err)
	require.Nil(t, in)

	outList, err := rs.ListSIPOutboundTrunk(ctx, &livekit.ListSIPOutboundTrunkRequest{})
	require.NoError(t, err)
	require.Empty(t, outList.Items)

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
	listOld, err := rs.ListSIPTrunk(ctx, &livekit.ListSIPTrunkRequest{})
	require.NoError(t, err)
	require.Len(t, listOld.Items, 3)
	slices.SortFunc(listOld.Items, func(a, b *livekit.SIPTrunkInfo) int {
		return strings.Compare(a.Name, b.Name)
	})
	require.True(t, proto.Equal(inT.AsTrunkInfo(), listOld.Items[0]))
	require.True(t, proto.Equal(oldT, listOld.Items[1]))
	require.True(t, proto.Equal(outT.AsTrunkInfo(), listOld.Items[2]))

	listIn, err := rs.ListSIPInboundTrunk(ctx, &livekit.ListSIPInboundTrunkRequest{})
	require.NoError(t, err)
	require.Len(t, listIn.Items, 2)
	slices.SortFunc(listIn.Items, func(a, b *livekit.SIPInboundTrunkInfo) int {
		return strings.Compare(a.Name, b.Name)
	})
	require.True(t, proto.Equal(inT, listIn.Items[0]))
	require.True(t, proto.Equal(oldT.AsInbound(), listIn.Items[1]))

	listOut, err := rs.ListSIPOutboundTrunk(ctx, &livekit.ListSIPOutboundTrunkRequest{})
	require.NoError(t, err)
	require.Len(t, listOut.Items, 2)
	slices.SortFunc(listOut.Items, func(a, b *livekit.SIPOutboundTrunkInfo) int {
		return strings.Compare(a.Name, b.Name)
	})
	require.True(t, proto.Equal(oldT.AsOutbound(), listOut.Items[0]))
	require.True(t, proto.Equal(outT, listOut.Items[1]))

	// Deletion. Should not return error if not exists.
	err = rs.DeleteSIPTrunk(ctx, oldID)
	require.NoError(t, err)
	err = rs.DeleteSIPTrunk(ctx, oldID)
	require.NoError(t, err)

	// Other objects are still there.
	inT2, err = rs.LoadSIPInboundTrunk(ctx, inID)
	require.NoError(t, err)
	require.True(t, proto.Equal(inT, inT2))

	outT2, err = rs.LoadSIPOutboundTrunk(ctx, outID)
	require.NoError(t, err)
	require.True(t, proto.Equal(outT, outT2))

	// Delete the rest
	err = rs.DeleteSIPTrunk(ctx, inID)
	require.NoError(t, err)
	err = rs.DeleteSIPTrunk(ctx, outID)
	require.NoError(t, err)

	// Check everything is deleted.
	oldList, err = rs.ListSIPTrunk(ctx, &livekit.ListSIPTrunkRequest{})
	require.NoError(t, err)
	require.Empty(t, oldList.Items)

	inList, err = rs.ListSIPInboundTrunk(ctx, &livekit.ListSIPInboundTrunkRequest{})
	require.NoError(t, err)
	require.Empty(t, inList.Items)

	outList, err = rs.ListSIPOutboundTrunk(ctx, &livekit.ListSIPOutboundTrunkRequest{})
	require.NoError(t, err)
	require.Empty(t, outList.Items)

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

func TestSIPTrunkList(t *testing.T) {
	s := redisStoreDocker(t)

	testIter(t, func(ctx context.Context, id string) error {
		if strings.HasSuffix(id, "0") {
			return s.StoreSIPTrunk(ctx, &livekit.SIPTrunkInfo{
				SipTrunkId:     id,
				OutboundNumber: id,
			})
		}
		return s.StoreSIPInboundTrunk(ctx, &livekit.SIPInboundTrunkInfo{
			SipTrunkId: id,
			Numbers:    []string{id},
		})
	}, func(ctx context.Context, page *livekit.Pagination, ids []string) iters.PageIter[*livekit.SIPInboundTrunkInfo] {
		return livekit.ListPageIter(s.ListSIPInboundTrunk, &livekit.ListSIPInboundTrunkRequest{
			TrunkIds: ids, Page: page,
		})
	})
}

func TestSIPRuleList(t *testing.T) {
	s := redisStoreDocker(t)

	testIter(t, func(ctx context.Context, id string) error {
		return s.StoreSIPDispatchRule(ctx, &livekit.SIPDispatchRuleInfo{
			SipDispatchRuleId: id,
			TrunkIds:          []string{id},
		})
	}, func(ctx context.Context, page *livekit.Pagination, ids []string) iters.PageIter[*livekit.SIPDispatchRuleInfo] {
		return livekit.ListPageIter(s.ListSIPDispatchRule, &livekit.ListSIPDispatchRuleRequest{
			DispatchRuleIds: ids, Page: page,
		})
	})
}

type listItem interface {
	ID() string
}

func allIDs[T listItem](t testing.TB, it iters.PageIter[T]) []string {
	defer it.Close()
	got, err := iters.AllPages(context.Background(), iters.MapPage(it, func(ctx context.Context, v T) (string, error) {
		return v.ID(), nil
	}))
	require.NoError(t, err)
	return got
}

func testIter[T listItem](
	t *testing.T,
	create func(ctx context.Context, id string) error,
	list func(ctx context.Context, page *livekit.Pagination, ids []string) iters.PageIter[T],
) {
	ctx := context.Background()
	var all []string
	for i := 0; i < 250; i++ {
		id := fmt.Sprintf("%05d", i)
		all = append(all, id)
		err := create(ctx, id)
		require.NoError(t, err)
	}

	// List everything with pagination disabled (legacy)
	it := list(ctx, nil, nil)
	got := allIDs(t, it)
	require.Equal(t, all, got)

	// List with pagination enabled
	it = list(ctx, &livekit.Pagination{Limit: 10}, nil)
	got = allIDs(t, it)
	require.Equal(t, all, got)

	// List with pagination enabled, custom ID
	it = list(ctx, &livekit.Pagination{Limit: 10, AfterId: all[55]}, nil)
	got = allIDs(t, it)
	require.Equal(t, all[56:], got)

	// List fixed IDs
	it = list(ctx, &livekit.Pagination{Limit: 10, AfterId: all[5]}, []string{
		all[10],
		all[3],
		"invalid",
		all[8],
	})
	got = allIDs(t, it)
	require.Equal(t, []string{
		all[8],
		all[10],
	}, got)
}
