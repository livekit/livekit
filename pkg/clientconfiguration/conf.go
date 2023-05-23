package clientconfiguration

import (
	"github.com/livekit/protocol/livekit"
)

// StaticConfigurations list specific device-side limitations that should be disabled at a global level
var StaticConfigurations = []ConfigurationItem{
	// {
	// 	Match:         &ScriptMatch{Expr: `c.protocol <= 5 || c.browser == "firefox"`},
	// 	Configuration: &livekit.ClientConfiguration{ResumeConnection: livekit.ClientConfigSetting_DISABLED},
	// 	Merge:         false,
	// },
	// {
	// 	Match: &ScriptMatch{Expr: `c.browser == "safari" && c.os == "ios"`},
	// 	Configuration: &livekit.ClientConfiguration{DisabledCodecs: &livekit.DisabledCodecs{Codecs: []*livekit.Codec{
	// 		{Mime: "video/vp9"},
	// 	}}},
	// 	Merge: false,
	// },
	{
		Match: &ScriptMatch{Expr: `c.device_model == "Xiaomi 2201117TI" && c.os == "android"`},
		Configuration: &livekit.ClientConfiguration{
			DisabledCodecs: &livekit.DisabledCodecs{
				Publish: []*livekit.Codec{{Mime: "video/h264"}},
			},
		},
		Merge: false,
	},
}
