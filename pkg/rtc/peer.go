package rtc

import (
	"context"
	"sync"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"
)

type WebRTCPeer struct {
	id          string
	conn        *webrtc.PeerConnection
	ctx         context.Context
	cancel      context.CancelFunc
	mediaEngine *MediaEngine

	lock           sync.RWMutex
	receiverConfig ReceiverConfig
	tracks         []*PeerTrack // tracks that the peer is publishing

	// callbacks & handlers
	onClose     func(*WebRTCPeer)
	onPeerTrack func(*WebRTCPeer, *PeerTrack)
}

func NewWebRTCPeer(id string, me *MediaEngine, conf WebRTCConfig) (*WebRTCPeer, error) {
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
		cancel:         cancel,
		mediaEngine:    me,
		lock:           sync.RWMutex{},
		receiverConfig: conf.receiver,
		tracks:         make([]*PeerTrack, 0),
	}

	pc.OnTrack(peer.onTrack)

	// TODO: handle data channel

	return peer, nil
}

func (p *WebRTCPeer) ID() string {
	return p.id
}

func (p *WebRTCPeer) Close() error {
	if p.ctx.Err() != nil {
		return p.ctx.Err()
	}
	// TODO: notify handler of closure
	if p.onClose != nil {
		p.onClose(p)
	}
	p.cancel()
	return p.conn.Close()
}

func (p *WebRTCPeer) OnClose(f func(*WebRTCPeer)) {
	p.onClose = f
}

func (p *WebRTCPeer) OnPeerTrack(f func(*WebRTCPeer, *PeerTrack)) {
	p.onPeerTrack = f
}

// when a new track is created, creates a PeerTrack and adds it to room
func (p *WebRTCPeer) onTrack(track *webrtc.Track, rtpReceiver *webrtc.RTPReceiver) {

	// create Receiver
	receiver := NewReceiver(p.ctx, rtpReceiver, p.receiverConfig, p.mediaEngine)
	pt := NewPeerTrack(p.id, track, receiver)

	p.lock.Lock()
	defer p.lock.Unlock()
	p.tracks = append(p.tracks, pt)

	if p.onPeerTrack != nil {
		// caller should hook up what happens when the peer track is available
		p.onPeerTrack(p, pt)
	}
}

func (p *WebRTCPeer) rtcpSendWorker() {
	t := time.NewTicker(time.Second)
	for {
		select {
		case <-t.C:
			pkts := make([]rtcp.Packet, 0)
			p.lock.RLock()
			for _, r := range p.tracks {
				rr, ps := r.receiver.BuildRTCP()
				if rr.SSRC != 0 {
					ps = append(ps, &rtcp.ReceiverReport{
						Reports: []rtcp.ReceptionReport{rr},
					})
				}
				pkts = append(pkts, ps...)
			}
			p.lock.RUnlock()
			if len(pkts) > 0 {
				if err := p.conn.WriteRTCP(pkts); err != nil {
					// TODO: log error
					//log.Errorf("write rtcp err: %v", err)
				}
			}
		case <-p.ctx.Done():
			t.Stop()
			return
		}
	}
}
