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
	"github.com/livekit/protocol/codecs/mime"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/utils/must"
)

// StaticConfigurations list specific device-side limitations that should be disabled at a global level
var StaticConfigurations = []ConfigurationItem{
	// {
	// 	Match:         must.Get(NewScriptMatch(`c.protocol <= 5 || c.browser == "firefox"`)),
	// 	Configuration: &livekit.ClientConfiguration{ResumeConnection: livekit.ClientConfigSetting_DISABLED},
	// 	Merge:         false,
	// },
	{
		Match: must.Get(NewScriptMatch(`c.browser == "safari"`)),
		Configuration: &livekit.ClientConfiguration{
			DisabledCodecs: &livekit.DisabledCodecs{
				Codecs: []*livekit.Codec{
					{Mime: mime.MimeTypeAV1.String()},
				},
			},
		},
		Merge: true,
	},
	{
		Match: must.Get(NewScriptMatch(`c.browser == "safari" && c.browser_version > "18.3"`)),
		Configuration: &livekit.ClientConfiguration{
			DisabledCodecs: &livekit.DisabledCodecs{
				Publish: []*livekit.Codec{
					{Mime: mime.MimeTypeVP9.String()},
				},
			},
		},
		Merge: true,
	},
	// Firefox 155 enabled AV1 for WebRTC by default, but 155/156 decode only the base spatial
	// layer of an AV1 SVC stream. SVC is not visible in SDP, so such a subscriber negotiating AV1
	// would be forwarded the SVC stream in place of the publisher's backup codec and render
	// higher layers black. Firefox 157 decodes the higher layers, so it keeps AV1.
	// Firefox does not publish AV1 either (no SVC encoding), so disable it in both directions.
	{
		Match: must.Get(NewScriptMatch(`(c.browser == "firefox" || c.browser == "firefox mobile") && c.browser_version < "157"`)),
		Configuration: &livekit.ClientConfiguration{
			DisabledCodecs: &livekit.DisabledCodecs{
				Codecs: []*livekit.Codec{
					{Mime: mime.MimeTypeAV1.String()},
				},
			},
		},
		Merge: true,
	},
	{
		Match: must.Get(NewScriptMatch(`(c.device_model == "xiaomi 2201117ti" && c.os == "android") ||
		  ((c.browser == "firefox" || c.browser == "firefox mobile") && c.os == "android")`)),
		Configuration: &livekit.ClientConfiguration{
			DisabledCodecs: &livekit.DisabledCodecs{
				Publish: []*livekit.Codec{
					{Mime: mime.MimeTypeH264.String()},
				},
			},
		},
		// merged so Firefox for Android keeps the AV1 rule above
		Merge: true,
	},
}
