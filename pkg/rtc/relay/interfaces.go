package relay

import (
	"context"

	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
)

type Relay interface {
	ID() string
	GetBufferFactory() *buffer.Factory
	Offer(signalFn func(signal []byte) ([]byte, error)) error
	Answer(request []byte) ([]byte, error)
	WriteRTCP(pkts []rtcp.Packet) error
	AddTrack(ctx context.Context, track webrtc.TrackLocal, trackRid string, trackMeta []byte) (*webrtc.RTPSender, error)
	RemoveTrack(sender *webrtc.RTPSender) error
	OnReady(f func())
	OnTrack(f func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver, mid string, rid string, trackMeta []byte))
	OnConnectionStateChange(f func(state webrtc.ICEConnectionState))
	OnMessage(func(id uint64, payload []byte))
	SendMessage(payload []byte) error
	SendReplyMessage(replyForID uint64, payload []byte) error
	SendMessageAndExpectReply(payload []byte) (<-chan []byte, error)
	DebugInfo() map[string]interface{}
	Close()
}
