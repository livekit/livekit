package clientconfiguration

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/livekit"
)

func TestScriptMatchConfiguration(t *testing.T) {
	t.Run("no merge", func(t *testing.T) {
		confs := []ConfigurationItem{
			{
				Match: &ScriptMatch{Expr: `c.protocol > 5 && c.browser != "firefox"`},
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
				Match: &ScriptMatch{Expr: `c.protocol > 5 && c.browser != "firefox"`},
				Configuration: &livekit.ClientConfiguration{
					ResumeConnection: livekit.ClientConfigSetting_ENABLED,
				},
				Merge: true,
			},
			{
				Match: &ScriptMatch{Expr: `c.sdk == "ANDROID"`},
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
		{name: "combined condition", expr: `c.protocol > 5 && (c.sdk=="ANDROID" || c.sdk=="IOS")`, result: true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			match := &ScriptMatch{Expr: c.expr}
			m, err := match.Match(client)
			if c.err {
				require.Error(t, err)
			} else {
				require.Equal(t, c.result, m)
			}
		})

	}
}
