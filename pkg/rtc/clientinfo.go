package rtc

import (
	"strconv"
	"strings"

	"github.com/livekit/protocol/livekit"
)

type ClientInfo struct {
	*livekit.ClientInfo
}

func (c ClientInfo) SupportsAudioRED() bool {
	return c.ClientInfo != nil && c.ClientInfo.Browser != "firefox" && c.ClientInfo.Browser != "safari"
}

// CompareVersion compares two semver versions
// returning 1 if current version is greater than version
// 0 if they are the same, and -1 if it's an earlier version
func (c ClientInfo) CompareVersion(version string) int {
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

func (c ClientInfo) SupportsICETCP() bool {
	if c.ClientInfo == nil {
		return false
	}
	if c.ClientInfo.Sdk == livekit.ClientInfo_GO {
		return false
	}
	if c.ClientInfo.Sdk == livekit.ClientInfo_SWIFT {
		// ICE/TCP added in 1.0.5
		return c.CompareVersion("1.0.5") >= 0
	}
	// most SDKs support ICE/TCP
	return true
}
