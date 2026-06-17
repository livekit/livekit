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
	"path/filepath"
	"sync/atomic"
	"time"

	"buf.build/go/protoyaml"
	"github.com/fsnotify/fsnotify"
	"github.com/pkg/errors"
	"gopkg.in/yaml.v3"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/psrpc"
)

var (
	ErrReadOnlySIPStore = psrpc.NewErrorf(psrpc.FailedPrecondition, "SIP config store is read-only")
)

const (
	DefaultSIPConfigPath = "sip_config.yaml"
	ConfigReloadInterval = 30 * time.Second // Periodic reload as fallback
)

type sipConfigFile struct {
	InboundTrunks  []*livekit.SIPInboundTrunkInfo  `yaml:"inbound_trunks,omitempty"`
	OutboundTrunks []*livekit.SIPOutboundTrunkInfo `yaml:"outbound_trunks,omitempty"`
	DispatchRules  []*livekit.SIPDispatchRuleInfo  `yaml:"dispatch_rules,omitempty"`
}

type FileSIPStore struct {
	configPath string
	config     atomic.Pointer[sipConfigFile]
	lastMod    atomic.Int64 // Unix timestamp of last modification
	watcher    *fsnotify.Watcher
	cancel     context.CancelFunc
}

func NewFileSIPStore(configPath string) (*FileSIPStore, error) {
	if configPath == "" {
		configPath = DefaultSIPConfigPath
	}

	store := FileSIPStore{
		configPath: configPath,
	}

	err := store.loadConfigFile()
	if err != nil {
		return nil, err
	}

	return &store, nil
}

func (s *FileSIPStore) Start(ctx context.Context) error {
	s.Stop()

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return errors.Wrap(err, "failed to create file watcher")
	}

	// Watch the directory containing the file to handle ConfigMap updates
	// ConfigMaps use symlinks that get atomically swapped, so we need to watch the parent dir
	configDir := filepath.Dir(s.configPath)
	if err := watcher.Add(configDir); err != nil {
		watcher.Close()
		return errors.Wrapf(err, "failed to watch directory %s", configDir)
	}

	watchCtx, cancel := context.WithCancel(ctx)
	s.watcher = watcher
	s.cancel = cancel

	go s.watchLoop(watchCtx)

	logger.Infow("started watching SIP config file", "path", s.configPath, "watchDir", configDir)
	return nil
}

func (s *FileSIPStore) Stop() {
	if s.cancel != nil {
		s.cancel()
	}
	if s.watcher != nil {
		s.watcher.Close()
	}
}

func (s *FileSIPStore) watchLoop(ctx context.Context) {
	configFileName := filepath.Base(s.configPath)
	ticker := time.NewTicker(ConfigReloadInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Periodic reload as fallback in case file watcher misses events
			// Only reload if file has actually changed
			if info, err := os.Stat(s.configPath); err == nil {
				modTime := info.ModTime().Unix()
				if modTime > s.lastMod.Load() {
					logger.Infow("periodic SIP config reload detected change", "path", s.configPath)
					if err := s.loadConfigFile(); err != nil {
						logger.Errorw("failed to periodically reload SIP config file", err, "path", s.configPath)
					}
				}
			}
		case event, ok := <-s.watcher.Events:
			if !ok {
				return
			}
			// Only process events for our config file (important when watching directory for ConfigMap support)
			eventFileName := filepath.Base(event.Name)
			if eventFileName != configFileName {
				continue
			}

			// ConfigMap updates typically trigger CREATE (symlink created) or WRITE events
			if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) {
				logger.Infow("SIP config file changed, reloading", "path", s.configPath, "event", event.Op.String())
				if err := s.loadConfigFile(); err != nil {
					logger.Errorw("failed to reload SIP config file", err, "path", s.configPath)
				} else {
					logger.Infow("SIP config file reloaded successfully", "path", s.configPath)
				}
			}
		case err, ok := <-s.watcher.Errors:
			if !ok {
				return
			}
			logger.Errorw("file watcher error", err)
		}
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
	config := s.config.Load()

	// Try inbound trunk
	for _, trunk := range config.InboundTrunks {
		if trunk.SipTrunkId == sipTrunkID {
			return trunk.AsTrunkInfo(), nil
		}
	}

	// Try outbound trunk
	for _, trunk := range config.OutboundTrunks {
		if trunk.SipTrunkId == sipTrunkID {
			return trunk.AsTrunkInfo(), nil
		}
	}

	return nil, ErrSIPTrunkNotFound
}

func (s *FileSIPStore) LoadSIPInboundTrunk(ctx context.Context, sipTrunkID string) (*livekit.SIPInboundTrunkInfo, error) {
	config := s.config.Load()

	for _, trunk := range config.InboundTrunks {
		if trunk.SipTrunkId == sipTrunkID {
			return trunk, nil
		}
	}

	return nil, ErrSIPTrunkNotFound
}

func (s *FileSIPStore) LoadSIPOutboundTrunk(ctx context.Context, sipTrunkID string) (*livekit.SIPOutboundTrunkInfo, error) {
	config := s.config.Load()

	for _, trunk := range config.OutboundTrunks {
		if trunk.SipTrunkId == sipTrunkID {
			return trunk, nil
		}
	}

	return nil, ErrSIPTrunkNotFound
}

func (s *FileSIPStore) ListSIPTrunk(ctx context.Context, req *livekit.ListSIPTrunkRequest) (*livekit.ListSIPTrunkResponse, error) {
	config := s.config.Load()

	var items []*livekit.SIPTrunkInfo

	for _, t := range config.InboundTrunks {
		v := t.AsTrunkInfo()
		if req.Filter(v) && req.Page.Filter(v) {
			items = append(items, v)
		}
	}

	for _, t := range config.OutboundTrunks {
		v := t.AsTrunkInfo()
		if req.Filter(v) && req.Page.Filter(v) {
			items = append(items, v)
		}
	}

	items = sortPage(items, req.Page)
	return &livekit.ListSIPTrunkResponse{Items: items}, nil
}

func (s *FileSIPStore) ListSIPInboundTrunk(ctx context.Context, req *livekit.ListSIPInboundTrunkRequest) (*livekit.ListSIPInboundTrunkResponse, error) {
	config := s.config.Load()

	var items []*livekit.SIPInboundTrunkInfo

	for _, t := range config.InboundTrunks {
		if req.Filter(t) && req.Page.Filter(t) {
			items = append(items, t)
		}
	}

	items = sortPage(items, req.Page)
	return &livekit.ListSIPInboundTrunkResponse{Items: items}, nil
}

func (s *FileSIPStore) ListSIPOutboundTrunk(ctx context.Context, req *livekit.ListSIPOutboundTrunkRequest) (*livekit.ListSIPOutboundTrunkResponse, error) {
	config := s.config.Load()

	var items []*livekit.SIPOutboundTrunkInfo

	for _, t := range config.OutboundTrunks {
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
	config := s.config.Load()

	for _, rule := range config.DispatchRules {
		if rule.SipDispatchRuleId == sipDispatchRuleID {
			return rule, nil
		}
	}

	return nil, ErrSIPDispatchRuleNotFound
}

func (s *FileSIPStore) ListSIPDispatchRule(ctx context.Context, req *livekit.ListSIPDispatchRuleRequest) (*livekit.ListSIPDispatchRuleResponse, error) {
	config := s.config.Load()

	var items []*livekit.SIPDispatchRuleInfo

	for _, rule := range config.DispatchRules {
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

func (s *FileSIPStore) loadConfigFile() error {
	fileInfo, err := os.Stat(s.configPath)
	if err != nil {
		return errors.Wrapf(err, "failed to stat %s", s.configPath)
	}

	data, err := os.ReadFile(s.configPath)
	if err != nil {
		return errors.Wrapf(err, "failed to read %s", s.configPath)
	}

	var rawConfig struct {
		InboundTrunks  []yaml.Node `yaml:"inbound_trunks,omitempty"`
		OutboundTrunks []yaml.Node `yaml:"outbound_trunks,omitempty"`
		DispatchRules  []yaml.Node `yaml:"dispatch_rules,omitempty"`
	}

	if err := yaml.Unmarshal(data, &rawConfig); err != nil {
		return errors.Wrapf(err, "failed to parse %s", s.configPath)
	}

	inboundTrunks, err := unmarshalSection[livekit.SIPInboundTrunkInfo](rawConfig.InboundTrunks)
	if err != nil {
		return err
	}

	outboundTrunks, err := unmarshalSection[livekit.SIPOutboundTrunkInfo](rawConfig.OutboundTrunks)
	if err != nil {
		return err
	}

	dispatchRules, err := unmarshalSection[livekit.SIPDispatchRuleInfo](rawConfig.OutboundTrunks)
	if err != nil {
		return err
	}

	s.config.Store(&sipConfigFile{
		InboundTrunks:  inboundTrunks,
		OutboundTrunks: outboundTrunks,
		DispatchRules:  dispatchRules,
	})

	// Update modification time
	s.lastMod.Store(fileInfo.ModTime().Unix())

	return nil
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
