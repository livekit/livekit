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

package rtc

import (
	"strconv"
	"strings"

	"github.com/livekit/protocol/livekit"
)

type ClientInfo struct {
	*livekit.ClientInfo
}

func (c ClientInfo) isFirefox() bool {
	return c.ClientInfo != nil && (strings.EqualFold(c.ClientInfo.Browser, "firefox") || strings.EqualFold(c.ClientInfo.Browser, "firefox mobile"))
}

func (c ClientInfo) isSafari() bool {
	return c.ClientInfo != nil && strings.EqualFold(c.ClientInfo.Browser, "safari")
}

func (c ClientInfo) isGo() bool {
	return c.ClientInfo != nil && c.ClientInfo.Sdk == livekit.ClientInfo_GO
}

func (c ClientInfo) isLinux() bool {
	return c.ClientInfo != nil && strings.EqualFold(c.ClientInfo.Os, "linux")
}

func (c ClientInfo) isAndroid() bool {
	return c.ClientInfo != nil && strings.EqualFold(c.ClientInfo.Os, "android")
}

func (c ClientInfo) isOBS() bool {
	return c.ClientInfo != nil && strings.Contains(c.ClientInfo.Browser, "OBS")
}

func (c ClientInfo) SupportsAudioRED() bool {
	return !c.isFirefox() && !c.isSafari()
}

func (c ClientInfo) SupportsPrflxOverRelay() bool {
	return !c.isFirefox()
}

// GoSDK(pion) relies on rtp packets to fire ontrack event, browsers and native (libwebrtc) rely on sdp
func (c ClientInfo) FireTrackByRTPPacket() bool {
	return c.isGo()
}

func (c ClientInfo) SupportsCodecChange() bool {
	return c.ClientInfo != nil && c.ClientInfo.Sdk != livekit.ClientInfo_GO && c.ClientInfo.Sdk != livekit.ClientInfo_UNKNOWN
}

func (c ClientInfo) CanHandleReconnectResponse() bool {
	if c.Sdk == livekit.ClientInfo_JS {
		// JS handles Reconnect explicitly in 1.6.3, prior to 1.6.4 it could not handle unknown responses
		if c.compareVersion("1.6.3") < 0 {
			return false
		}
	}
	return true
}

func (c ClientInfo) SupportsICETCP() bool {
	if c.ClientInfo == nil {
		return false
	}
	if c.ClientInfo.Sdk == livekit.ClientInfo_GO {
		// Go does not support active TCP
		return false
	}
	if c.ClientInfo.Sdk == livekit.ClientInfo_SWIFT {
		// ICE/TCP added in 1.0.5
		return c.compareVersion("1.0.5") >= 0
	}
	// most SDKs support ICE/TCP
	return true
}

func (c ClientInfo) SupportsChangeRTPSenderEncodingActive() bool {
	return !c.isFirefox()
}

func (c ClientInfo) ComplyWithCodecOrderInSDPAnswer() bool {
	return !((c.isLinux() || c.isAndroid()) && c.isFirefox())
}

// Rust SDK can't decode unknown signal message (TrackSubscribed and ErrorResponse)
func (c ClientInfo) SupportsTrackSubscribedEvent() bool {
	return !(c.ClientInfo.GetSdk() == livekit.ClientInfo_RUST && c.ClientInfo.GetProtocol() < 10)
}

func (c ClientInfo) SupportsRequestResponse() bool {
	return c.SupportsTrackSubscribedEvent()
}

func (c ClientInfo) SupportsSctpZeroChecksum() bool {
	return !(c.ClientInfo.GetSdk() == livekit.ClientInfo_UNKNOWN ||
		(c.isGo() && c.compareVersion("2.4.0") < 0))
}

// compareVersion compares a semver against the current client SDK version
// returning 1 if current version is greater than version
// 0 if they are the same, and -1 if it's an earlier version
func (c ClientInfo) compareVersion(version string) int {
	if c.ClientInfo == nil {
		return -1
	}
	parts0 := strings.Split(c.ClientInfo.Version, ".")
	parts1 := strings.Split(version, ".")
	ints0 := make([]int, 3)
	ints1 := make([]int, 3)
	for i := 0; i < 3; i++ {
		if len(parts0) > i {
			ints0[i], _ = strconv.Atoi(parts0[i])
		}
		if len(parts1) > i {
			ints1[i], _ = strconv.Atoi(parts1[i])
		}
		if ints0[i] > ints1[i] {
			return 1
		} else if ints0[i] < ints1[i] {
			return -1
		}
	}
	return 0
}
