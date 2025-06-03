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
)

const (
	SIPTrunkKey         = "sip_trunk"
	SIPInboundTrunkKey  = "sip_inbound_trunk"
	SIPOutboundTrunkKey = "sip_outbound_trunk"
	SIPDispatchRuleKey  = "sip_dispatch_rule"
)

func (s *RedisStore) StoreSIPTrunk(ctx context.Context, info *livekit.SIPTrunkInfo) error {
	return redisStoreOne(s.ctx, s, SIPTrunkKey, info.SipTrunkId, info)
}

func (s *RedisStore) StoreSIPInboundTrunk(ctx context.Context, info *livekit.SIPInboundTrunkInfo) error {
	return redisStoreOne(s.ctx, s, SIPInboundTrunkKey, info.SipTrunkId, info)
}

func (s *RedisStore) StoreSIPOutboundTrunk(ctx context.Context, info *livekit.SIPOutboundTrunkInfo) error {
	return redisStoreOne(s.ctx, s, SIPOutboundTrunkKey, info.SipTrunkId, info)
}

func (s *RedisStore) loadSIPLegacyTrunk(ctx context.Context, id string) (*livekit.SIPTrunkInfo, error) {
	return redisLoadOne[livekit.SIPTrunkInfo](ctx, s, SIPTrunkKey, id, ErrSIPTrunkNotFound)
}

func (s *RedisStore) loadSIPInboundTrunk(ctx context.Context, id string) (*livekit.SIPInboundTrunkInfo, error) {
	return redisLoadOne[livekit.SIPInboundTrunkInfo](ctx, s, SIPInboundTrunkKey, id, ErrSIPTrunkNotFound)
}

func (s *RedisStore) loadSIPOutboundTrunk(ctx context.Context, id string) (*livekit.SIPOutboundTrunkInfo, error) {
	return redisLoadOne[livekit.SIPOutboundTrunkInfo](ctx, s, SIPOutboundTrunkKey, id, ErrSIPTrunkNotFound)
}

func (s *RedisStore) LoadSIPTrunk(ctx context.Context, id string) (*livekit.SIPTrunkInfo, error) {
	tr, err := s.loadSIPLegacyTrunk(ctx, id)
	if err == nil {
		return tr, nil
	} else if err != ErrSIPTrunkNotFound {
		return nil, err
	}
	in, err := s.loadSIPInboundTrunk(ctx, id)
	if err == nil {
		return in.AsTrunkInfo(), nil
	} else if err != ErrSIPTrunkNotFound {
		return nil, err
	}
	out, err := s.loadSIPOutboundTrunk(ctx, id)
	if err == nil {
		return out.AsTrunkInfo(), nil
	} else if err != ErrSIPTrunkNotFound {
		return nil, err
	}
	return nil, ErrSIPTrunkNotFound
}

func (s *RedisStore) LoadSIPInboundTrunk(ctx context.Context, id string) (*livekit.SIPInboundTrunkInfo, error) {
	in, err := s.loadSIPInboundTrunk(ctx, id)
	if err == nil {
		return in, nil
	} else if err != ErrSIPTrunkNotFound {
		return nil, err
	}
	tr, err := s.loadSIPLegacyTrunk(ctx, id)
	if err == nil {
		return tr.AsInbound(), nil
	} else if err != ErrSIPTrunkNotFound {
		return nil, err
	}
	return nil, ErrSIPTrunkNotFound
}

func (s *RedisStore) LoadSIPOutboundTrunk(ctx context.Context, id string) (*livekit.SIPOutboundTrunkInfo, error) {
	in, err := s.loadSIPOutboundTrunk(ctx, id)
	if err == nil {
		return in, nil
	} else if err != ErrSIPTrunkNotFound {
		return nil, err
	}
	tr, err := s.loadSIPLegacyTrunk(ctx, id)
	if err == nil {
		return tr.AsOutbound(), nil
	} else if err != ErrSIPTrunkNotFound {
		return nil, err
	}
	return nil, ErrSIPTrunkNotFound
}

func (s *RedisStore) DeleteSIPTrunk(ctx context.Context, id string) error {
	err1 := s.rc.HDel(s.ctx, SIPTrunkKey, id).Err()
	err2 := s.rc.HDel(s.ctx, SIPInboundTrunkKey, id).Err()
	err3 := s.rc.HDel(s.ctx, SIPOutboundTrunkKey, id).Err()
	if err1 != nil {
		return err1
	}
	if err2 != nil {
		return err2
	}
	if err3 != nil {
		return err3
	}
	return nil
}

func (s *RedisStore) listSIPLegacyTrunk(ctx context.Context, page *livekit.Pagination) ([]*livekit.SIPTrunkInfo, error) {
	return redisIterPage[livekit.SIPTrunkInfo](ctx, s, SIPTrunkKey, page)
}

func (s *RedisStore) listSIPInboundTrunk(ctx context.Context, page *livekit.Pagination) ([]*livekit.SIPInboundTrunkInfo, error) {
	return redisIterPage[livekit.SIPInboundTrunkInfo](ctx, s, SIPInboundTrunkKey, page)
}

func (s *RedisStore) listSIPOutboundTrunk(ctx context.Context, page *livekit.Pagination) ([]*livekit.SIPOutboundTrunkInfo, error) {
	return redisIterPage[livekit.SIPOutboundTrunkInfo](ctx, s, SIPOutboundTrunkKey, page)
}

func (s *RedisStore) listSIPDispatchRule(ctx context.Context, page *livekit.Pagination) ([]*livekit.SIPDispatchRuleInfo, error) {
	return redisIterPage[livekit.SIPDispatchRuleInfo](ctx, s, SIPDispatchRuleKey, page)
}

func (s *RedisStore) ListSIPTrunk(ctx context.Context, req *livekit.ListSIPTrunkRequest) (*livekit.ListSIPTrunkResponse, error) {
	var items []*livekit.SIPTrunkInfo
	old, err := s.listSIPLegacyTrunk(ctx, req.Page)
	if err != nil {
		return nil, err
	}
	for _, t := range old {
		v := t
		if req.Filter(v) && req.Page.Filter(v) {
			items = append(items, v)
		}
	}
	in, err := s.listSIPInboundTrunk(ctx, req.Page)
	if err != nil {
		return nil, err
	}
	for _, t := range in {
		v := t.AsTrunkInfo()
		if req.Filter(v) && req.Page.Filter(v) {
			items = append(items, v)
		}
	}
	out, err := s.listSIPOutboundTrunk(ctx, req.Page)
	if err != nil {
		return nil, err
	}
	for _, t := range out {
		v := t.AsTrunkInfo()
		if req.Filter(v) && req.Page.Filter(v) {
			items = append(items, v)
		}
	}
	items = sortPage(items, req.Page)
	return &livekit.ListSIPTrunkResponse{Items: items}, nil
}

func (s *RedisStore) ListSIPInboundTrunk(ctx context.Context, req *livekit.ListSIPInboundTrunkRequest) (*livekit.ListSIPInboundTrunkResponse, error) {
	var items []*livekit.SIPInboundTrunkInfo
	in, err := s.listSIPInboundTrunk(ctx, req.Page)
	if err != nil {
		return nil, err
	}
	for _, t := range in {
		v := t
		if req.Filter(v) && req.Page.Filter(v) {
			items = append(items, v)
		}
	}
	old, err := s.listSIPLegacyTrunk(ctx, req.Page)
	if err != nil {
		return nil, err
	}
	for _, t := range old {
		v := t.AsInbound()
		if req.Filter(v) && req.Page.Filter(v) {
			items = append(items, v)
		}
	}
	items = sortPage(items, req.Page)
	return &livekit.ListSIPInboundTrunkResponse{Items: items}, nil
}

func (s *RedisStore) ListSIPOutboundTrunk(ctx context.Context, req *livekit.ListSIPOutboundTrunkRequest) (*livekit.ListSIPOutboundTrunkResponse, error) {
	var items []*livekit.SIPOutboundTrunkInfo
	out, err := s.listSIPOutboundTrunk(ctx, req.Page)
	if err != nil {
		return nil, err
	}
	for _, t := range out {
		v := t
		if req.Filter(v) && req.Page.Filter(v) {
			items = append(items, v)
		}
	}
	old, err := s.listSIPLegacyTrunk(ctx, req.Page)
	if err != nil {
		return nil, err
	}
	for _, t := range old {
		v := t.AsOutbound()
		if req.Filter(v) && req.Page.Filter(v) {
			items = append(items, v)
		}
	}
	items = sortPage(items, req.Page)
	return &livekit.ListSIPOutboundTrunkResponse{Items: items}, nil
}

func (s *RedisStore) StoreSIPDispatchRule(ctx context.Context, info *livekit.SIPDispatchRuleInfo) error {
	return redisStoreOne(ctx, s, SIPDispatchRuleKey, info.SipDispatchRuleId, info)
}

func (s *RedisStore) LoadSIPDispatchRule(ctx context.Context, sipDispatchRuleId string) (*livekit.SIPDispatchRuleInfo, error) {
	return redisLoadOne[livekit.SIPDispatchRuleInfo](ctx, s, SIPDispatchRuleKey, sipDispatchRuleId, ErrSIPDispatchRuleNotFound)
}

func (s *RedisStore) DeleteSIPDispatchRule(ctx context.Context, sipDispatchRuleId string) error {
	return s.rc.HDel(s.ctx, SIPDispatchRuleKey, sipDispatchRuleId).Err()
}

func (s *RedisStore) ListSIPDispatchRule(ctx context.Context, req *livekit.ListSIPDispatchRuleRequest) (*livekit.ListSIPDispatchRuleResponse, error) {
	var items []*livekit.SIPDispatchRuleInfo
	out, err := s.listSIPDispatchRule(ctx, req.Page)
	if err != nil {
		return nil, err
	}
	for _, t := range out {
		v := t
		if req.Filter(v) && req.Page.Filter(v) {
			items = append(items, v)
		}
	}
	items = sortPage(items, req.Page)
	return &livekit.ListSIPDispatchRuleResponse{Items: items}, nil
}
