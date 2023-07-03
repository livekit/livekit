package relay

import (
	"context"

	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
)

type Relay interface {
	GetBufferFactory() *buffer.Factory
	Offer(signalFn func(signal []byte) ([]byte, error)) error
	Answer(request []byte) ([]byte, error)
	WriteRTCP(pkts []rtcp.Packet) error
	AddTrack(ctx context.Context, track webrtc.TrackLocal, trackRid string, trackMeta string) (*webrtc.RTPSender, error)
	OnReady(f func())
	OnTrack(f func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver, mid string, rid string, trackMeta string))
	OnConnectionStateChange(f func(state webrtc.ICEConnectionState))
	Send(payload []byte) error
	SendReply(replyForID uint64, payload []byte) error
	SendAndExpectReply(payload []byte) (<-chan []byte, error)
	DebugInfo() map[string]interface{}
}
