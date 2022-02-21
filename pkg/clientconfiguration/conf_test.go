package clientconfiguration

import (
	"testing"

	"github.com/livekit/protocol/livekit"
	"github.com/stretchr/testify/require"
)

func TestScriptMatchConfiguration(t *testing.T) {
	t.Run("no merge", func(t *testing.T) {
		confs := []ConfigurationItem{
			{
				Match: &ScriptMatch{Expr: `c.protocol > 5 && c.browser != "firefox"`},
				Configuration: &livekit.RTCClientConfiguration{
					Signal: &livekit.SignalConfiguration{Migration: true},
				},
			},
		}

		cm := NewStaticClientConfigurationManager(confs)

		conf := cm.GetConfiguration(&livekit.ClientInfo{Protocol: 4})
		require.Nil(t, conf)

		conf = cm.GetConfiguration(&livekit.ClientInfo{Protocol: 6, Browser: "firefox"})
		require.Nil(t, conf)

		conf = cm.GetConfiguration(&livekit.ClientInfo{Protocol: 6, Browser: "chrome"})
		require.True(t, conf.Signal.Migration)
	})

	t.Run("merge", func(t *testing.T) {
		confs := []ConfigurationItem{
			{
				Match: &ScriptMatch{Expr: `c.protocol > 5 && c.browser != "firefox"`},
				Configuration: &livekit.RTCClientConfiguration{
					Signal: &livekit.SignalConfiguration{Migration: true},
				},
				Merge: true,
			},
			{
				Match: &ScriptMatch{Expr: `c.sdk == "ANDROID"`},
				Configuration: &livekit.RTCClientConfiguration{
					Video: &livekit.VideoConfiguration{
						PreferredCodec: &livekit.Codec{Mime: "video/h264"},
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
		require.True(t, conf.Signal.Migration)
		require.Equal(t, "video/h264", conf.Video.PreferredCodec.Mime)
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
