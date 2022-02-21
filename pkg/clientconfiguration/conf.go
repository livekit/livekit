package clientconfiguration

import "github.com/livekit/protocol/livekit"

// configurations for livekit-client, add more configuration to StaticConfigurations as need
var StaticConfigurations = []ConfigurationItem{
	{
		Match: &ScriptMatch{Expr: `c.protocol <= 5 || c.browser == "firefox"`},
		Configuration: &livekit.RTCClientConfiguration{
			Signal: &livekit.SignalConfiguration{Migration: false},
		},
	},
}
