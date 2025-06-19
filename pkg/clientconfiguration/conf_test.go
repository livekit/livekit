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

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/utils/must"
)

func TestScriptMatchConfiguration(t *testing.T) {
	t.Run("no merge", func(t *testing.T) {
		confs := []ConfigurationItem{
			{
				Match: must.Get(NewScriptMatch(`c.protocol > 5 && c.browser != "firefox"`)),
				Configuration: &livekit.ClientConfiguration{
					ResumeConnection: livekit.ClientConfigSetting_ENABLED,
				},
			},
		}

		cm := NewStaticClientConfigurationManager(confs)

		conf := cm.GetConfiguration(&livekit.ClientInfo{Protocol: 4})
		require.Nil(t, conf)

		conf = cm.GetConfiguration(&livekit.ClientInfo{Protocol: 6, Browser: "firefox"})
		require.Nil(t, conf)

		conf = cm.GetConfiguration(&livekit.ClientInfo{Protocol: 6, Browser: "chrome"})
		require.Equal(t, conf.ResumeConnection, livekit.ClientConfigSetting_ENABLED)
	})

	t.Run("merge", func(t *testing.T) {
		confs := []ConfigurationItem{
			{
				Match: must.Get(NewScriptMatch(`c.protocol > 5 && c.browser != "firefox"`)),
				Configuration: &livekit.ClientConfiguration{
					ResumeConnection: livekit.ClientConfigSetting_ENABLED,
				},
				Merge: true,
			},
			{
				Match: must.Get(NewScriptMatch(`c.sdk == "android"`)),
				Configuration: &livekit.ClientConfiguration{
					Video: &livekit.VideoConfiguration{
						HardwareEncoder: livekit.ClientConfigSetting_DISABLED,
					},
				},
				Merge: true,
			},
		}

		cm := NewStaticClientConfigurationManager(confs)

		conf := cm.GetConfiguration(&livekit.ClientInfo{Protocol: 4})
		require.Nil(t, conf)

		conf = cm.GetConfiguration(&livekit.ClientInfo{Protocol: 6, Browser: "firefox"})
		require.Nil(t, conf)

		conf = cm.GetConfiguration(&livekit.ClientInfo{Protocol: 6, Browser: "chrome", Sdk: 3})
		require.Equal(t, conf.ResumeConnection, livekit.ClientConfigSetting_ENABLED)
		require.Equal(t, conf.Video.HardwareEncoder, livekit.ClientConfigSetting_DISABLED)
	})
}

func TestScriptMatch(t *testing.T) {
	client := &livekit.ClientInfo{
		Protocol:    6,
		Browser:     "chrome",
		Sdk:         3, // android
		DeviceModel: "12345",
	}

	type testcase struct {
		name   string
		expr   string
		result bool
		err    bool
	}

	cases := []testcase{
		{name: "simple match", expr: `c.protocol > 5`, result: true},
		{name: "invalid expr", expr: `cc.protocol > 5`, err: true},
		{name: "unexist field", expr: `c.protocols > 5`, err: true},
		{name: "combined condition", expr: `c.protocol > 5 && (c.sdk=="android" || c.sdk=="ios")`, result: true},
		{name: "combined condition2", expr: `(c.device_model == "xiaomi 2201117ti" && c.os == "android") || ((c.browser == "firefox" || c.browser == "firefox mobile") && (c.os == "linux" || c.os == "android"))`, result: false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			match, err := NewScriptMatch(c.expr)
			if err != nil {
				if !c.err {
					require.NoError(t, err)
				}
				return
			}
			m, err := match.Match(client)
			if c.err {
				require.Error(t, err)
			} else {
				require.Equal(t, c.result, m)
			}
		})

	}
}
