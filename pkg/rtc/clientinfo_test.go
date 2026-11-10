/*
 * Copyright 2022 LiveKit, Inc
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package rtc

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/livekit"
)

func TestClientInfo_CompareVersion(t *testing.T) {
	c := ClientInfo{
		ClientInfo: &livekit.ClientInfo{
			Version: "1",
		},
	}
	require.Equal(t, 1, c.compareVersion("0.1.0"))
	require.Equal(t, 0, c.compareVersion("1.0.0"))
	require.Equal(t, -1, c.compareVersion("1.0.5"))
}

func TestClientInfo_SupportsICETCP(t *testing.T) {
	t.Run("GO SDK cannot support TCP", func(t *testing.T) {
		c := ClientInfo{
			ClientInfo: &livekit.ClientInfo{
				Sdk: livekit.ClientInfo_GO,
			},
		}
		require.False(t, c.SupportsICETCP())
	})

	t.Run("Swift SDK cannot support TCP before 1.0.5", func(t *testing.T) {
		c := ClientInfo{
			ClientInfo: &livekit.ClientInfo{
				Sdk:     livekit.ClientInfo_SWIFT,
				Version: "1.0.4",
			},
		}
		require.False(t, c.SupportsICETCP())
		c.Version = "1.0.5"
		require.True(t, c.SupportsICETCP())
	})
}

func TestClientInfo_WebKitFeatures(t *testing.T) {
	// Browser / Os values are what the server's user agent parser sets for JS clients
	testCases := []struct {
		name             string
		sdk              livekit.ClientInfo_SDK
		browser          string
		os               string
		transceiverReuse bool
		audioRED         bool
	}{
		{"macOS Safari", livekit.ClientInfo_JS, "Safari", "Mac OS X", false, false},
		{"iPadOS Safari (desktop UA)", livekit.ClientInfo_JS, "Safari", "Mac OS X", false, false},
		{"iOS Safari", livekit.ClientInfo_JS, "Mobile Safari", "iOS", false, false},
		{"iOS WKWebView", livekit.ClientInfo_JS, "Mobile Safari UI/WKWebView", "iOS", false, false},
		{"iOS Chrome", livekit.ClientInfo_JS, "Chrome Mobile iOS", "iOS", false, false},
		{"iOS Firefox", livekit.ClientInfo_JS, "Firefox iOS", "iOS", false, false},
		{"iOS Edge", livekit.ClientInfo_JS, "Edge Mobile", "iOS", false, false},
		{"iOS in-app browser", livekit.ClientInfo_JS, "Instagram", "iOS", false, false},
		{"desktop Chrome", livekit.ClientInfo_JS, "Chrome", "Mac OS X", true, true},
		{"desktop Firefox", livekit.ClientInfo_JS, "Firefox", "Windows", true, false},
		{"Android Chrome", livekit.ClientInfo_JS, "Chrome Mobile", "Android", true, true},
		{"React Native iOS", livekit.ClientInfo_REACT_NATIVE, "LiveKitExample", "ios", true, true},
		{"Swift", livekit.ClientInfo_SWIFT, "", "ios", true, true},
		{"Flutter iOS", livekit.ClientInfo_FLUTTER, "Dart", "ios", true, true},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			c := ClientInfo{
				ClientInfo: &livekit.ClientInfo{
					Sdk:     tc.sdk,
					Browser: tc.browser,
					Os:      tc.os,
				},
			}
			require.Equal(t, tc.transceiverReuse, c.SupportsTransceiverReuse())
			require.Equal(t, tc.audioRED, c.SupportsAudioRED())
		})
	}

	require.True(t, ClientInfo{}.SupportsTransceiverReuse())
	require.True(t, ClientInfo{}.SupportsAudioRED())
}
