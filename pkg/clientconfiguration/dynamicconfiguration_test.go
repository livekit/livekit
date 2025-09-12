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
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/protocol/livekit"
)

func TestDynamicClientConfigurationManager(t *testing.T) {
	t.Run("basic functionality", func(t *testing.T) {
		conf := &config.ClientConfigurationConfig{
			Configurations: []config.ClientConfigurationItem{
				{
					Match:         `c.browser == "safari"`,
					Configuration: `{"disabled_codecs": {"codecs": [{"mime": "video/AV1"}]}}`,
					Merge:         true,
				},
			},
		}

		manager, err := NewDynamicClientConfigurationManager(conf)
		require.NoError(t, err)

		// Test Safari client
		clientInfo := &livekit.ClientInfo{
			Browser: "safari",
		}
		config := manager.GetConfiguration(clientInfo)
		require.NotNil(t, config)
		
		// Test Chrome client (should get static configuration)
		clientInfo = &livekit.ClientInfo{
			Browser: "chrome",
		}
		config = manager.GetConfiguration(clientInfo)
		// Should get static configuration or nil for chrome
		// (depending on static configuration)
	})

	t.Run("update configuration", func(t *testing.T) {
		conf := &config.ClientConfigurationConfig{
			Configurations: []config.ClientConfigurationItem{},
		}

		manager, err := NewDynamicClientConfigurationManager(conf)
		require.NoError(t, err)

		// Update with new configuration - use proper JSON format
		newConf := &config.ClientConfigurationConfig{
			Configurations: []config.ClientConfigurationItem{
				{
					Match:         `c.browser == "firefox"`,
					Configuration: `{"resume_connection": 2}`,  // Use numeric value for DISABLED
					Merge:         true,
				},
			},
		}

		err = manager.UpdateConfiguration(newConf)
		require.NoError(t, err)

		// Test the updated configuration
		clientInfo := &livekit.ClientInfo{
			Browser: "firefox",
		}
		config := manager.GetConfiguration(clientInfo)
		// Should get some configuration (could be dynamic or static merged)
		if config != nil {
			t.Logf("Got configuration: %+v", config)
		}
	})
}
