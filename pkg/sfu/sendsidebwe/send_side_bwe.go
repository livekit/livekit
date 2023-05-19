package sendsidebwe

import "github.com/livekit/protocol/logger"

type SendSideBWE struct {
	*TransportWideExtension
	*PacketTracker
}

func NewSendSideBWE(logger logger.Logger) *SendSideBWE {
	return &SendSideBWE{
		TransportWideExtension: NewTransportWideExtension(),
		PacketTracker:          NewPacketTracker(),
	}
}

// ------------------------------------------------
