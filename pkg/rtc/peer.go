package rtc

import (
	"context"
	"sync"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"
	"github.com/pkg/errors"

	"github.com/livekit/livekit-server/pkg/logger"
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
	once           sync.Once

	// callbacks & handlers
	// OnPeerTrack - remote peer added a track
	OnPeerTrack func(*WebRTCPeer, *PeerTrack)
	// OnOffer - offer is ready for remote peer
	OnOffer func(webrtc.SessionDescription)
	// OnIceCandidate - ice candidate discovered for local peer
	OnICECandidate func(c *webrtc.ICECandidateInit)
	OnClose        func(*WebRTCPeer)
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

	pc.OnNegotiationNeeded(func() {
		offer, err := pc.CreateOffer(nil)
		if err != nil {
			// TODO: log
			return
		}

		err = pc.SetLocalDescription(offer)
		if err != nil {
			// TODO: log
			return
		}
		if peer.OnOffer != nil {
			peer.OnOffer(offer)
		}
	})

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		if peer.OnICECandidate != nil {
			ci := c.ToJSON()
			peer.OnICECandidate(&ci)
		}
	})

	// TODO: handle data channel

	return peer, nil
}

func (p *WebRTCPeer) ID() string {
	return p.id
}

// Answer an offer from remote peer
func (p *WebRTCPeer) Answer(sdp webrtc.SessionDescription) (answer webrtc.SessionDescription, err error) {
	if err = p.SetRemoteDescription(sdp); err != nil {
		return
	}

	answer, err = p.conn.CreateAnswer(nil)
	if err != nil {
		err = errors.Wrap(err, "could not create answer")
		return
	}

	if err = p.conn.SetLocalDescription(answer); err != nil {
		err = errors.Wrap(err, "could not set local description")
		return
	}

	return
}

// SetRemoteDescription when receiving an answer from remote
func (p *WebRTCPeer) SetRemoteDescription(sdp webrtc.SessionDescription) error {
	if err := p.conn.SetRemoteDescription(sdp); err != nil {
		return errors.Wrap(err, "could not set remote description")
	}
	return nil
}

// AddICECandidate adds candidates for remote peer
func (p *WebRTCPeer) AddICECandidate(candidate webrtc.ICECandidateInit) error {
	if err := p.conn.AddICECandidate(candidate); err != nil {
		return err
	}
	return nil
}

func (p *WebRTCPeer) Start() {
	p.once.Do(func() {
		go p.rtcpSendWorker()
	})
}

func (p *WebRTCPeer) Close() error {
	if p.ctx.Err() != nil {
		return p.ctx.Err()
	}
	if p.OnClose != nil {
		p.OnClose(p)
	}
	p.cancel()
	return p.conn.Close()
}

// Subscribes otherPeer to all of the tracks
func (p *WebRTCPeer) AddSubscriber(otherPeer *WebRTCPeer) error {
	p.lock.RLock()
	defer p.lock.RUnlock()

	for _, track := range p.tracks {
		if err := track.AddSubscriber(otherPeer); err != nil {
			return err
		}
	}
	return nil
}

func (p *WebRTCPeer) RemoveSubscriber(peerId string) {
	p.lock.RLock()
	defer p.lock.RUnlock()

	for _, track := range p.tracks {
		track.RemoveSubscriber(peerId)
	}
}

// when a new track is created, creates a PeerTrack and adds it to room
func (p *WebRTCPeer) onTrack(track *webrtc.Track, rtpReceiver *webrtc.RTPReceiver) {

	// create Receiver
	receiver := NewReceiver(p.ctx, p.id, rtpReceiver, p.receiverConfig, p.mediaEngine)
	pt := NewPeerTrack(p.ctx, p.id, p.conn, track, receiver)

	p.lock.Lock()
	p.tracks = append(p.tracks, pt)
	p.lock.Unlock()

	if p.OnPeerTrack != nil {
		// caller should hook up what happens when the peer track is available
		go p.OnPeerTrack(p, pt)
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
					logger.GetLogger().Errorw("error writing RTCP to peer",
						"peer", p.id,
						"err", err,
					)
				}
			}
		case <-p.ctx.Done():
			t.Stop()
			return
		}
	}
}
