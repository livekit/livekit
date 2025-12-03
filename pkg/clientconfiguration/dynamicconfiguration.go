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

package clientconfiguration

import (
	"encoding/json"
	"sync"

	"google.golang.org/protobuf/proto"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/utils"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	protoutils "github.com/livekit/protocol/utils"
)

type DynamicClientConfigurationManager struct {
	mu             sync.RWMutex
	staticManager  *StaticClientConfigurationManager
	dynamicConfigs []ConfigurationItem
}

func NewDynamicClientConfigurationManager(conf *config.ClientConfigurationConfig) (*DynamicClientConfigurationManager, error) {
	manager := &DynamicClientConfigurationManager{
		staticManager: NewStaticClientConfigurationManager(StaticConfigurations),
	}

	if err := manager.UpdateConfiguration(conf); err != nil {
		return nil, err
	}

	return manager, nil
}

func (d *DynamicClientConfigurationManager) UpdateConfiguration(conf *config.ClientConfigurationConfig) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	var configs []ConfigurationItem
	for _, item := range conf.Configurations {
		match, err := NewScriptMatch(item.Match)
		if err != nil {
			logger.Errorw("failed to parse match rule", err, "match", item.Match)
			continue
		}

		var clientConfig livekit.ClientConfiguration
		if err := json.Unmarshal([]byte(item.Configuration), &clientConfig); err != nil {
			logger.Errorw("failed to parse client configuration", err, "configuration", item.Configuration)
			continue
		}

		configs = append(configs, ConfigurationItem{
			Match:         match,
			Configuration: &clientConfig,
			Merge:         item.Merge,
		})
	}

	d.dynamicConfigs = configs
	return nil
}

func (d *DynamicClientConfigurationManager) GetConfiguration(clientInfo *livekit.ClientInfo) *livekit.ClientConfiguration {
	d.mu.RLock()
	defer d.mu.RUnlock()

	// First check dynamic configurations
	dynamicConfig := d.getDynamicConfiguration(clientInfo)
	
	// Then check static configurations
	staticConfig := d.staticManager.GetConfiguration(clientInfo)

	// Merge configurations if both exist
	if dynamicConfig != nil && staticConfig != nil {
		merged := protoutils.CloneProto(staticConfig)
		proto.Merge(merged, dynamicConfig)
		return merged
	} else if dynamicConfig != nil {
		return dynamicConfig
	} else if staticConfig != nil {
		return staticConfig
	}

	return nil
}

func (d *DynamicClientConfigurationManager) getDynamicConfiguration(clientInfo *livekit.ClientInfo) *livekit.ClientConfiguration {
	var matchedConf []*livekit.ClientConfiguration
	for _, c := range d.dynamicConfigs {
		matched, err := c.Match.Match(clientInfo)
		if err != nil {
			logger.Errorw("matchrule failed", err,
				"clientInfo", logger.Proto(utils.ClientInfoWithoutAddress(clientInfo)),
			)
			continue
		}
		if !matched {
			continue
		}
		if !c.Merge {
			return c.Configuration
		}
		matchedConf = append(matchedConf, c.Configuration)
	}

	var conf *livekit.ClientConfiguration
	for k, v := range matchedConf {
		if k == 0 {
			conf = protoutils.CloneProto(matchedConf[0])
		} else {
			// TODO : there is a problem use protobuf merge, we don't have flag to indicate 'no value',
			// don't override default behavior or other configuration's field. So a bool value = false or
			// a int value = 0 will override same field in other configuration
			proto.Merge(conf, v)
		}
	}
	return conf
}
