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
	"os"

	"buf.build/go/protoyaml"
	"github.com/pkg/errors"
	"gopkg.in/yaml.v3"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/psrpc"
)

var (
	ErrReadOnlySIPStore = psrpc.NewErrorf(psrpc.FailedPrecondition, "SIP config store is read-only")
)

const (
	DefaultSIPConfigPath = "sip_config.yaml"
)

type rawConfig struct {
	InboundTrunks  []yaml.Node `yaml:"inbound_trunks,omitempty"`
	OutboundTrunks []yaml.Node `yaml:"outbound_trunks,omitempty"`
	DispatchRules  []yaml.Node `yaml:"dispatch_rules,omitempty"`
}

type FileSIPStore struct {
	ConfigPath string
}

func NewFileSIPStore(configPath string) *FileSIPStore {
	if configPath == "" {
		configPath = DefaultSIPConfigPath
	}

	return &FileSIPStore{
		ConfigPath: configPath,
	}
}

func (s *FileSIPStore) StoreSIPTrunk(ctx context.Context, info *livekit.SIPTrunkInfo) error {
	return ErrReadOnlySIPStore
}

func (s *FileSIPStore) StoreSIPInboundTrunk(ctx context.Context, info *livekit.SIPInboundTrunkInfo) error {
	return ErrReadOnlySIPStore
}

func (s *FileSIPStore) StoreSIPOutboundTrunk(ctx context.Context, info *livekit.SIPOutboundTrunkInfo) error {
	return ErrReadOnlySIPStore
}

func (s *FileSIPStore) LoadSIPTrunk(ctx context.Context, sipTrunkID string) (*livekit.SIPTrunkInfo, error) {
	// Try inbound trunk
	inboundTrunks, err := s.loadInboundTrunks()
	if err != nil {
		return nil, err
	}

	for _, trunk := range inboundTrunks {
		if trunk.SipTrunkId == sipTrunkID {
			return trunk.AsTrunkInfo(), nil
		}
	}

	// Try outbound trunk
	outboundTrunks, err := s.loadOutboundTrunks()
	if err != nil {
		return nil, err
	}

	for _, trunk := range outboundTrunks {
		if trunk.SipTrunkId == sipTrunkID {
			return trunk.AsTrunkInfo(), nil
		}
	}

	return nil, ErrSIPTrunkNotFound
}

func (s *FileSIPStore) LoadSIPInboundTrunk(ctx context.Context, sipTrunkID string) (*livekit.SIPInboundTrunkInfo, error) {
	inboundTrunks, err := s.loadInboundTrunks()
	if err != nil {
		return nil, err
	}

	for _, trunk := range inboundTrunks {
		if trunk.SipTrunkId == sipTrunkID {
			return trunk, nil
		}
	}

	return nil, ErrSIPTrunkNotFound
}

func (s *FileSIPStore) LoadSIPOutboundTrunk(ctx context.Context, sipTrunkID string) (*livekit.SIPOutboundTrunkInfo, error) {
	outboundTrunks, err := s.loadOutboundTrunks()
	if err != nil {
		return nil, err
	}

	for _, trunk := range outboundTrunks {
		if trunk.SipTrunkId == sipTrunkID {
			return trunk, nil
		}
	}

	return nil, ErrSIPTrunkNotFound
}

func (s *FileSIPStore) ListSIPTrunk(ctx context.Context, req *livekit.ListSIPTrunkRequest) (*livekit.ListSIPTrunkResponse, error) {
	var items []*livekit.SIPTrunkInfo

	inboundTrunks, err := s.loadInboundTrunks()
	if err != nil {
		return nil, err
	}

	for _, t := range inboundTrunks {
		v := t.AsTrunkInfo()
		if req.Filter(v) && req.Page.Filter(v) {
			items = append(items, v)
		}
	}

	outboundTrunks, err := s.loadOutboundTrunks()
	if err != nil {
		return nil, err
	}

	for _, t := range outboundTrunks {
		v := t.AsTrunkInfo()
		if req.Filter(v) && req.Page.Filter(v) {
			items = append(items, v)
		}
	}

	items = sortPage(items, req.Page)
	return &livekit.ListSIPTrunkResponse{Items: items}, nil
}

func (s *FileSIPStore) ListSIPInboundTrunk(ctx context.Context, req *livekit.ListSIPInboundTrunkRequest) (*livekit.ListSIPInboundTrunkResponse, error) {
	inboundTrunks, err := s.loadInboundTrunks()
	if err != nil {
		return nil, err
	}

	var items []*livekit.SIPInboundTrunkInfo

	for _, t := range inboundTrunks {
		if req.Filter(t) && req.Page.Filter(t) {
			items = append(items, t)
		}
	}

	items = sortPage(items, req.Page)
	return &livekit.ListSIPInboundTrunkResponse{Items: items}, nil
}

func (s *FileSIPStore) ListSIPOutboundTrunk(ctx context.Context, req *livekit.ListSIPOutboundTrunkRequest) (*livekit.ListSIPOutboundTrunkResponse, error) {
	outboundTrunks, err := s.loadOutboundTrunks()
	if err != nil {
		return nil, err
	}

	var items []*livekit.SIPOutboundTrunkInfo

	for _, t := range outboundTrunks {
		if req.Filter(t) && req.Page.Filter(t) {
			items = append(items, t)
		}
	}

	items = sortPage(items, req.Page)
	return &livekit.ListSIPOutboundTrunkResponse{Items: items}, nil
}

func (s *FileSIPStore) DeleteSIPTrunk(ctx context.Context, sipTrunkID string) error {
	return ErrReadOnlySIPStore
}

func (s *FileSIPStore) StoreSIPDispatchRule(ctx context.Context, info *livekit.SIPDispatchRuleInfo) error {
	return ErrReadOnlySIPStore
}

func (s *FileSIPStore) LoadSIPDispatchRule(ctx context.Context, sipDispatchRuleID string) (*livekit.SIPDispatchRuleInfo, error) {
	dispatchRules, err := s.loadDispatchRules()
	if err != nil {
		return nil, err
	}

	for _, rule := range dispatchRules {
		if rule.SipDispatchRuleId == sipDispatchRuleID {
			return rule, nil
		}
	}

	return nil, ErrSIPDispatchRuleNotFound
}

func (s *FileSIPStore) ListSIPDispatchRule(ctx context.Context, req *livekit.ListSIPDispatchRuleRequest) (*livekit.ListSIPDispatchRuleResponse, error) {
	dispatchRules, err := s.loadDispatchRules()
	if err != nil {
		return nil, err
	}

	var items []*livekit.SIPDispatchRuleInfo

	for _, rule := range dispatchRules {
		if req.Filter(rule) && req.Page.Filter(rule) {
			items = append(items, rule)
		}
	}

	items = sortPage(items, req.Page)
	return &livekit.ListSIPDispatchRuleResponse{Items: items}, nil
}

func (s *FileSIPStore) DeleteSIPDispatchRule(ctx context.Context, sipDispatchRuleID string) error {
	return ErrReadOnlySIPStore
}

func (s *FileSIPStore) loadInboundTrunks() ([]*livekit.SIPInboundTrunkInfo, error) {
	config, err := s.loadRawConfigFile()
	if err != nil {
		return nil, err
	}

	inboundTrunks, err := unmarshalSection[livekit.SIPInboundTrunkInfo](config.InboundTrunks)
	if err != nil {
		return nil, err
	}

	return inboundTrunks, nil
}

func (s *FileSIPStore) loadOutboundTrunks() ([]*livekit.SIPOutboundTrunkInfo, error) {
	config, err := s.loadRawConfigFile()
	if err != nil {
		return nil, err
	}

	outboundTrunks, err := unmarshalSection[livekit.SIPOutboundTrunkInfo](config.OutboundTrunks)
	if err != nil {
		return nil, err
	}

	return outboundTrunks, nil
}

func (s *FileSIPStore) loadDispatchRules() ([]*livekit.SIPDispatchRuleInfo, error) {
	config, err := s.loadRawConfigFile()
	if err != nil {
		return nil, err
	}

	dispatchRules, err := unmarshalSection[livekit.SIPDispatchRuleInfo](config.OutboundTrunks)
	if err != nil {
		return nil, err
	}

	return dispatchRules, nil
}

func (s *FileSIPStore) loadRawConfigFile() (*rawConfig, error) {
	if _, err := os.Stat(s.ConfigPath); os.IsNotExist(err) {
		// File doesn't exist, return empty config (not an error)
		return &rawConfig{}, nil
	}

	data, err := os.ReadFile(s.ConfigPath)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to read %s", s.ConfigPath)
	}

	var config rawConfig
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, errors.Wrapf(err, "failed to parse %s", s.ConfigPath)
	}

	return &config, nil
}

func unmarshalSection[T any, P protoMsg[T]](section []yaml.Node) ([]P, error) {
	var result []P
	for _, node := range section {
		nodeData, err := yaml.Marshal(&node)
		if err != nil {
			return nil, errors.Wrap(err, "failed to marshal node")
		}

		var p P = new(T)
		if err := protoyaml.Unmarshal(nodeData, p); err != nil {
			return nil, errors.Wrap(err, "failed to unmarshal node")
		}
		result = append(result, p)
	}

	return result, nil
}
