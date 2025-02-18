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
	"github.com/dennwc/iters"
	"github.com/livekit/livekit-server/pkg/service"
	"github.com/livekit/psrpc"
	"slices"
	"testing"

	"github.com/livekit/protocol/livekit"
	"github.com/stretchr/testify/require"
)

func ioStoreDocker(t testing.TB) (*service.IOInfoService, *service.RedisStore) {
	r := redisClientDocker(t)
	bus := psrpc.NewRedisMessageBus(r)
	rs := service.NewRedisStore(r)
	io, err := service.NewIOInfoService(bus, rs, rs, rs, nil)
	require.NoError(t, err)
	return io, rs
}

func TestSIPTrunkSelect(t *testing.T) {
	ctx := context.Background()
	s, rs := ioStoreDocker(t)

	for _, tr := range []*livekit.SIPInboundTrunkInfo{
		{SipTrunkId: "any", Numbers: nil},
		{SipTrunkId: "B", Numbers: []string{"B1", "B2"}},
		{SipTrunkId: "BC", Numbers: []string{"B1", "C1"}},
	} {
		err := rs.StoreSIPInboundTrunk(ctx, tr)
		require.NoError(t, err)
	}

	for _, tr := range []*livekit.SIPTrunkInfo{
		{SipTrunkId: "old-any", OutboundNumber: ""},
		{SipTrunkId: "old-A", OutboundNumber: "A"},
	} {
		err := rs.StoreSIPTrunk(ctx, tr)
		require.NoError(t, err)
	}

	for _, c := range []struct {
		number string
		exp    []string
	}{
		{"A", []string{"old-A", "old-any", "any"}},
		{"B1", []string{"B", "BC", "old-any", "any"}},
		{"B2", []string{"B", "old-any", "any"}},
		{"C1", []string{"BC", "old-any", "any"}},
		{"wrong", []string{"old-any", "any"}},
	} {
		t.Run(c.number, func(t *testing.T) {
			it := s.SelectSIPInboundTrunk(ctx, c.number)
			defer it.Close()
			list, err := iters.All(it)
			require.NoError(t, err)
			var ids []string
			for _, v := range list {
				ids = append(ids, v.SipTrunkId)
			}
			slices.Sort(c.exp)
			slices.Sort(ids)
			require.Equal(t, c.exp, ids)
		})
	}
}

func TestSIPRuleSelect(t *testing.T) {
	ctx := context.Background()
	s, rs := ioStoreDocker(t)

	for _, r := range []*livekit.SIPDispatchRuleInfo{
		{SipDispatchRuleId: "any", TrunkIds: nil},
		{SipDispatchRuleId: "B", TrunkIds: []string{"B1", "B2"}},
		{SipDispatchRuleId: "BC", TrunkIds: []string{"B1", "C1"}},
	} {
		err := rs.StoreSIPDispatchRule(ctx, r)
		require.NoError(t, err)
	}

	for _, c := range []struct {
		trunk string
		exp   []string
	}{
		{"A", []string{"any"}},
		{"B1", []string{"B", "BC", "any"}},
		{"B2", []string{"B", "any"}},
		{"C1", []string{"BC", "any"}},
		{"wrong", []string{"any"}},
	} {
		t.Run(c.trunk, func(t *testing.T) {
			it := s.SelectSIPDispatchRule(ctx, c.trunk)
			defer it.Close()
			list, err := iters.All(it)
			require.NoError(t, err)
			var ids []string
			for _, v := range list {
				ids = append(ids, v.SipDispatchRuleId)
			}
			slices.Sort(c.exp)
			slices.Sort(ids)
			require.Equal(t, c.exp, ids)
		})
	}
}
