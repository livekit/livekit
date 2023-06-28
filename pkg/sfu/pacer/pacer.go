package pacer

import (
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
)

type ExtensionData struct {
	ID      uint8
	Payload []byte
}

type Packet struct {
	Header             *rtp.Header
	Extensions         []ExtensionData
	Payload            []byte
	AbsSendTimeExtID   uint8
	TransportWideExtID uint8
	WriteStream        webrtc.TrackLocalWriter
	Metadata           interface{}
	OnSent             func(md interface{}, sentHeader *rtp.Header, payloadSize int, sentTime time.Time, sendError error)
}

type Pacer interface {
	Enqueue(p Packet)
	Stop()
}

// ------------------------------------------------
