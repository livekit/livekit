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
	tx := s.rc.TxPipeline()
	tx.HDel(s.ctx, SIPTrunkKey, id)
	tx.HDel(s.ctx, SIPInboundTrunkKey, id)
	tx.HDel(s.ctx, SIPOutboundTrunkKey, id)
	_, err := tx.Exec(ctx)
	return err
}

func (s *RedisStore) listSIPLegacyTrunk(ctx context.Context) ([]*livekit.SIPTrunkInfo, error) {
	return redisLoadMany[livekit.SIPTrunkInfo](ctx, s, SIPTrunkKey)
}

func (s *RedisStore) listSIPInboundTrunk(ctx context.Context) ([]*livekit.SIPInboundTrunkInfo, error) {
	return redisLoadMany[livekit.SIPInboundTrunkInfo](ctx, s, SIPInboundTrunkKey)
}

func (s *RedisStore) listSIPOutboundTrunk(ctx context.Context) ([]*livekit.SIPOutboundTrunkInfo, error) {
	return redisLoadMany[livekit.SIPOutboundTrunkInfo](ctx, s, SIPOutboundTrunkKey)
}

func (s *RedisStore) ListSIPTrunk(ctx context.Context) ([]*livekit.SIPTrunkInfo, error) {
	infos, err := s.listSIPLegacyTrunk(ctx)
	if err != nil {
		return nil, err
	}
	in, err := s.listSIPInboundTrunk(ctx)
	if err != nil {
		return infos, err
	}
	for _, t := range in {
		infos = append(infos, t.AsTrunkInfo())
	}
	out, err := s.listSIPOutboundTrunk(ctx)
	if err != nil {
		return infos, err
	}
	for _, t := range out {
		infos = append(infos, t.AsTrunkInfo())
	}
	return infos, nil
}

func (s *RedisStore) ListSIPInboundTrunk(ctx context.Context) (infos []*livekit.SIPInboundTrunkInfo, err error) {
	in, err := s.listSIPInboundTrunk(ctx)
	if err != nil {
		return in, err
	}
	old, err := s.listSIPLegacyTrunk(ctx)
	if err != nil {
		return nil, err
	}
	for _, t := range old {
		in = append(in, t.AsInbound())
	}
	return in, nil
}

func (s *RedisStore) ListSIPOutboundTrunk(ctx context.Context) (infos []*livekit.SIPOutboundTrunkInfo, err error) {
	out, err := s.listSIPOutboundTrunk(ctx)
	if err != nil {
		return out, err
	}
	old, err := s.listSIPLegacyTrunk(ctx)
	if err != nil {
		return nil, err
	}
	for _, t := range old {
		out = append(out, t.AsOutbound())
	}
	return out, nil
}

func (s *RedisStore) StoreSIPDispatchRule(ctx context.Context, info *livekit.SIPDispatchRuleInfo) error {
	return redisStoreOne(ctx, s, SIPDispatchRuleKey, info.SipDispatchRuleId, info)
}

func (s *RedisStore) LoadSIPDispatchRule(ctx context.Context, sipDispatchRuleId string) (*livekit.SIPDispatchRuleInfo, error) {
	return redisLoadOne[livekit.SIPDispatchRuleInfo](ctx, s, SIPDispatchRuleKey, sipDispatchRuleId, ErrSIPDispatchRuleNotFound)
}

func (s *RedisStore) DeleteSIPDispatchRule(ctx context.Context, info *livekit.SIPDispatchRuleInfo) error {
	return s.rc.HDel(s.ctx, SIPDispatchRuleKey, info.SipDispatchRuleId).Err()
}

func (s *RedisStore) ListSIPDispatchRule(ctx context.Context) (infos []*livekit.SIPDispatchRuleInfo, err error) {
	return redisLoadMany[livekit.SIPDispatchRuleInfo](ctx, s, SIPDispatchRuleKey)
}
