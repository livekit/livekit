package rtc

import "github.com/livekit/protocol/livekit"

type ClientInfo struct {
	*livekit.ClientInfo
}

func (c ClientInfo) SupportsAudioRED() bool {
	return c.ClientInfo.Browser != "firefox" && c.ClientInfo.Browser != "safari"
}
