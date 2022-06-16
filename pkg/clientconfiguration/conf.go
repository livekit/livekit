package clientconfiguration

// configurations for livekit-client, add more configuration to StaticConfigurations as need
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
}
