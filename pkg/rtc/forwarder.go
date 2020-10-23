package rtc

import (
	"github.com/pion/rtp"
)

// a forwarder publishes data to a target track or datachannel
// manages the RTCP loop with the target peer
type Forwarder interface {
	ChannelType() ChannelType
	WriteRTP(*rtp.Packet)
	Start() error
	Stop() error
}

type ChannelType int

const (
	ChannelAudio ChannelType = iota + 1
	ChannelVideo
	ChannelData
)
