package rtc

import (
	"context"

	"github.com/pion/webrtc/v3"
)

type WebRTCPeer struct {
	id          string
	conn        *webrtc.PeerConnection
	ctx         context.Context
	cancelFunc  context.CancelFunc
	mediaEngine MediaEngine

	receiverConfig ReceiverConfig

	// callbacks & handlers
	onPeerTrack func(*PeerTrack)
}

func NewWebRTCPeer(id string, me MediaEngine, conf WebRTCConfig) (*WebRTCPeer, error) {
	api := webrtc.NewAPI(webrtc.WithMediaEngine(me.MediaEngine), webrtc.WithSettingEngine(conf.setting))
	pc, err := api.NewPeerConnection(conf.configuration)

	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	peer := &WebRTCPeer{
		id:             id,
		conn:           pc,
		ctx:            ctx,
		cancelFunc:     cancel,
		mediaEngine:    me,
		receiverConfig: conf.receiver,
	}

	pc.OnTrack(peer.onTrack)

	return peer, nil
}

// when a new track is created, creates a PeerTrack and adds it to room
func (p *WebRTCPeer) onTrack(track *webrtc.Track, receiver *webrtc.RTPReceiver) {
	onPeerTrack := p.onPeerTrack
	if onPeerTrack != nil {

	}
}
