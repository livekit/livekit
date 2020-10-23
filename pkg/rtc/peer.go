package rtc

import (
	"context"

	"github.com/pion/webrtc/v3"
)

type WebRTCPeer struct {
	conn        *webrtc.PeerConnection
	ctx         context.Context
	cancelFunc  context.CancelFunc
	mediaEngine MediaEngine

	maxBandwidth  uint64
	maxBufferTime int

	// callbacks & handlers
	onPeerTrack func(*PeerTrack)
}

func NewWebRTCPeer(me MediaEngine, conf WebRTCConfig) (*WebRTCPeer, error) {
	api := webrtc.NewAPI(webrtc.WithMediaEngine(me.MediaEngine), webrtc.WithSettingEngine(conf.setting))
	pc, err := api.NewPeerConnection(conf.configuration)

	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	peer := &WebRTCPeer{
		conn:          pc,
		ctx:           ctx,
		cancelFunc:    cancel,
		mediaEngine:   me,
		maxBandwidth:  conf.maxBandwidth,
		maxBufferTime: conf.maxBufferTime,
	}

	pc.OnTrack(peer.onTrack)

	return peer, nil
}

// when a new track is created, creates a PeerTrack and adds it to room
func (p *WebRTCPeer) onTrack(track *webrtc.Track, receiver *webrtc.RTPReceiver) {

}
