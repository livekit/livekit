package sfu

import (
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/ion-sfu/pkg/buffer"
	"github.com/pion/ion-sfu/pkg/relay"
	"github.com/pion/rtcp"
	"github.com/pion/transport/packetio"
	"github.com/pion/webrtc/v3"
)

type Publisher struct {
	mu  sync.RWMutex
	id  string
	pc  *webrtc.PeerConnection
	cfg *WebRTCTransportConfig

	router     Router
	session    Session
	tracks     []PublisherTrack
	relayed    atomicBool
	relayPeers []*relayPeer
	candidates []webrtc.ICECandidateInit

	onICEConnectionStateChangeHandler atomic.Value // func(webrtc.ICEConnectionState)
	onPublisherTrack                  atomic.Value // func(PublisherTrack)

	closeOnce sync.Once
}

type relayPeer struct {
	peer                    *relay.Peer
	dcs                     []*webrtc.DataChannel
	withSRReports           bool
	relayFanOutDataChannels bool
}

type PublisherTrack struct {
	Track    *webrtc.TrackRemote
	Receiver Receiver
	// This will be used in the future for tracks that will be relayed as clients or servers
	// This is for SVC and Simulcast where you will be able to chose if the relayed peer just
	// want a single track (for recording/ processing) or get all the tracks (for load balancing)
	clientRelay bool
}

// NewPublisher creates a new Publisher
func NewPublisher(id string, session Session, cfg *WebRTCTransportConfig) (*Publisher, error) {
	me, err := getPublisherMediaEngine()
	if err != nil {
		Logger.Error(err, "NewPeer error", "peer_id", id)
		return nil, errPeerConnectionInitFailed
	}

	api := webrtc.NewAPI(webrtc.WithMediaEngine(me), webrtc.WithSettingEngine(cfg.Setting))
	pc, err := api.NewPeerConnection(cfg.Configuration)

	if err != nil {
		Logger.Error(err, "NewPeer error", "peer_id", id)
		return nil, errPeerConnectionInitFailed
	}

	p := &Publisher{
		id:      id,
		pc:      pc,
		cfg:     cfg,
		router:  newRouter(id, session, cfg),
		session: session,
	}

	pc.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		Logger.V(1).Info("Peer got remote track id",
			"peer_id", p.id,
			"track_id", track.ID(),
			"mediaSSRC", track.SSRC(),
			"rid", track.RID(),
			"stream_id", track.StreamID(),
		)

		r, pub := p.router.AddReceiver(receiver, track, track.ID(), track.StreamID())
		if pub {
			p.session.Publish(p.router, r)
			p.mu.Lock()
			publisherTrack := PublisherTrack{track, r, true}
			p.tracks = append(p.tracks, publisherTrack)
			for _, rp := range p.relayPeers {
				if err = p.createRelayTrack(track, r, rp.peer); err != nil {
					Logger.V(1).Error(err, "Creating relay track.", "peer_id", p.id)
				}
			}
			p.mu.Unlock()
			if handler, ok := p.onPublisherTrack.Load().(func(PublisherTrack)); ok && handler != nil {
				handler(publisherTrack)
			}
		} else {
			p.mu.Lock()
			p.tracks = append(p.tracks, PublisherTrack{track, r, false})
			p.mu.Unlock()
		}
	})

	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		if dc.Label() == APIChannelLabel {
			// terminate api data channel
			return
		}
		p.session.AddDatachannel(id, dc)
	})

	pc.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
		Logger.V(1).Info("ice connection status", "state", connectionState)
		switch connectionState {
		case webrtc.ICEConnectionStateFailed:
			fallthrough
		case webrtc.ICEConnectionStateClosed:
			Logger.V(1).Info("webrtc ice closed", "peer_id", p.id)
			p.Close()
		}

		if handler, ok := p.onICEConnectionStateChangeHandler.Load().(func(webrtc.ICEConnectionState)); ok && handler != nil {
			handler(connectionState)
		}
	})

	p.router.SetRTCPWriter(p.pc.WriteRTCP)

	return p, nil
}

func (p *Publisher) Answer(offer webrtc.SessionDescription) (webrtc.SessionDescription, error) {
	if err := p.pc.SetRemoteDescription(offer); err != nil {
		return webrtc.SessionDescription{}, err
	}

	for _, c := range p.candidates {
		if err := p.pc.AddICECandidate(c); err != nil {
			Logger.Error(err, "Add publisher ice candidate to peer err", "peer_id", p.id)
		}
	}
	p.candidates = nil

	answer, err := p.pc.CreateAnswer(nil)
	if err != nil {
		return webrtc.SessionDescription{}, err
	}
	if err := p.pc.SetLocalDescription(answer); err != nil {
		return webrtc.SessionDescription{}, err
	}
	return answer, nil
}

// GetRouter returns Router with mediaSSRC
func (p *Publisher) GetRouter() Router {
	return p.router
}

// Close peer
func (p *Publisher) Close() {
	p.closeOnce.Do(func() {
		if len(p.relayPeers) > 0 {
			p.mu.Lock()
			for _, rp := range p.relayPeers {
				if err := rp.peer.Close(); err != nil {
					Logger.Error(err, "Closing relay peer transport.")
				}
			}
			p.mu.Unlock()
		}
		p.router.Stop()
		if err := p.pc.Close(); err != nil {
			Logger.Error(err, "webrtc transport close err")
		}
	})
}

func (p *Publisher) OnPublisherTrack(f func(track PublisherTrack)) {
	p.onPublisherTrack.Store(f)
}

// OnICECandidate handler
func (p *Publisher) OnICECandidate(f func(c *webrtc.ICECandidate)) {
	p.pc.OnICECandidate(f)
}

func (p *Publisher) OnICEConnectionStateChange(f func(connectionState webrtc.ICEConnectionState)) {
	p.onICEConnectionStateChangeHandler.Store(f)
}

func (p *Publisher) SignalingState() webrtc.SignalingState {
	return p.pc.SignalingState()
}

func (p *Publisher) PeerConnection() *webrtc.PeerConnection {
	return p.pc
}

// Relay will relay all current and future tracks from current Publisher
func (p *Publisher) Relay(signalFn func(meta relay.PeerMeta, signal []byte) ([]byte, error),
	options ...func(r *relayPeer)) (*relay.Peer, error) {
	lrp := &relayPeer{}
	for _, o := range options {
		o(lrp)
	}

	rp, err := relay.NewPeer(relay.PeerMeta{
		PeerID:    p.id,
		SessionID: p.session.ID(),
	}, &relay.PeerConfig{
		SettingEngine: p.cfg.Setting,
		ICEServers:    p.cfg.Configuration.ICEServers,
		Logger:        Logger,
	})
	if err != nil {
		return nil, fmt.Errorf("relay: %w", err)
	}
	lrp.peer = rp

	rp.OnReady(func() {
		peer := p.session.GetPeer(p.id)

		p.relayed.set(true)
		if lrp.relayFanOutDataChannels {
			for _, lbl := range p.session.GetFanOutDataChannelLabels() {
				lbl := lbl
				dc, err := rp.CreateDataChannel(lbl)
				if err != nil {
					Logger.V(1).Error(err, "Creating data channels.", "peer_id", p.id)
				}
				dc.OnMessage(func(msg webrtc.DataChannelMessage) {
					if peer == nil || peer.Subscriber() == nil {
						return
					}
					if sdc := peer.Subscriber().DataChannel(lbl); sdc != nil {
						if msg.IsString {
							if err = sdc.SendText(string(msg.Data)); err != nil {
								Logger.Error(err, "Sending dc message err")
							}
						} else {
							if err = sdc.Send(msg.Data); err != nil {
								Logger.Error(err, "Sending dc message err")
							}
						}
					}
				})
			}
		}

		p.mu.Lock()
		for _, tp := range p.tracks {
			if !tp.clientRelay {
				// simulcast will just relay client track for now
				continue
			}
			if err = p.createRelayTrack(tp.Track, tp.Receiver, rp); err != nil {
				Logger.V(1).Error(err, "Creating relay track.", "peer_id", p.id)
			}
		}
		p.relayPeers = append(p.relayPeers, lrp)
		p.mu.Unlock()

		if lrp.withSRReports {
			go p.relayReports(rp)
		}
	})

	rp.OnDataChannel(func(channel *webrtc.DataChannel) {
		if !lrp.relayFanOutDataChannels {
			return
		}
		p.mu.Lock()
		lrp.dcs = append(lrp.dcs, channel)
		p.mu.Unlock()

		p.session.AddDatachannel("", channel)
	})

	if err = rp.Offer(signalFn); err != nil {
		return nil, fmt.Errorf("relay: %w", err)
	}

	return rp, nil
}

func (p *Publisher) PublisherTracks() []PublisherTrack {
	p.mu.Lock()
	defer p.mu.Unlock()

	tracks := make([]PublisherTrack, len(p.tracks))
	for idx, track := range p.tracks {
		tracks[idx] = track
	}
	return tracks
}

// AddRelayFanOutDataChannel adds fan out data channel to relayed peers
func (p *Publisher) AddRelayFanOutDataChannel(label string) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	for _, rp := range p.relayPeers {
		for _, dc := range rp.dcs {
			if dc.Label() == label {
				continue
			}
		}

		dc, err := rp.peer.CreateDataChannel(label)
		if err != nil {
			Logger.V(1).Error(err, "Creating data channels.", "peer_id", p.id)
		}
		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			p.session.FanOutMessage("", label, msg)
		})
	}
}

// GetRelayedDataChannels Returns a slice of data channels that belongs to relayed
// peers
func (p *Publisher) GetRelayedDataChannels(label string) []*webrtc.DataChannel {
	p.mu.RLock()
	defer p.mu.RUnlock()

	dcs := make([]*webrtc.DataChannel, 0, len(p.relayPeers))
	for _, rp := range p.relayPeers {
		for _, dc := range rp.dcs {
			if dc.Label() == label {
				dcs = append(dcs, dc)
				break
			}
		}
	}
	return dcs
}

// Relayed returns true if the publisher has been relayed at least once
func (p *Publisher) Relayed() bool {
	return p.relayed.get()
}

func (p *Publisher) Tracks() []*webrtc.TrackRemote {
	p.mu.RLock()
	defer p.mu.RUnlock()

	tracks := make([]*webrtc.TrackRemote, len(p.tracks))
	for idx, track := range p.tracks {
		tracks[idx] = track.Track
	}
	return tracks
}

// AddICECandidate to peer connection
func (p *Publisher) AddICECandidate(candidate webrtc.ICECandidateInit) error {
	if p.pc.RemoteDescription() != nil {
		return p.pc.AddICECandidate(candidate)
	}
	p.candidates = append(p.candidates, candidate)
	return nil
}

func (p *Publisher) createRelayTrack(track *webrtc.TrackRemote, receiver Receiver, rp *relay.Peer) error {
	codec := track.Codec()
	downTrack, err := NewDownTrack(webrtc.RTPCodecCapability{
		MimeType:     codec.MimeType,
		ClockRate:    codec.ClockRate,
		Channels:     codec.Channels,
		SDPFmtpLine:  codec.SDPFmtpLine,
		RTCPFeedback: []webrtc.RTCPFeedback{{"nack", ""}, {"nack", "pli"}},
	}, receiver, p.cfg.BufferFactory, p.id, p.cfg.Router.MaxPacketTrack)
	if err != nil {
		Logger.V(1).Error(err, "Create Relay downtrack err", "peer_id", p.id)
		return err
	}

	sdr, err := rp.AddTrack(receiver.(*WebRTCReceiver).receiver, track, downTrack)
	if err != nil {
		Logger.V(1).Error(err, "Relaying track.", "peer_id", p.id)
		return fmt.Errorf("relay: %w", err)
	}

	p.cfg.BufferFactory.GetOrNew(packetio.RTCPBufferPacket,
		uint32(sdr.GetParameters().Encodings[0].SSRC)).(*buffer.RTCPReader).OnPacket(func(bytes []byte) {
		pkts, err := rtcp.Unmarshal(bytes)
		if err != nil {
			Logger.V(1).Error(err, "Unmarshal rtcp reports", "peer_id", p.id)
			return
		}
		var rpkts []rtcp.Packet
		for _, pkt := range pkts {
			switch pk := pkt.(type) {
			case *rtcp.PictureLossIndication:
				rpkts = append(rpkts, &rtcp.PictureLossIndication{
					SenderSSRC: pk.MediaSSRC,
					MediaSSRC:  uint32(track.SSRC()),
				})
			}
		}

		if len(rpkts) > 0 {
			if err := p.pc.WriteRTCP(rpkts); err != nil {
				Logger.V(1).Error(err, "Sending rtcp relay reports", "peer_id", p.id)
			}
		}

	})

	downTrack.OnCloseHandler(func() {
		if err = sdr.Stop(); err != nil {
			Logger.V(1).Error(err, "Stopping relay sender.", "peer_id", p.id)
		}
	})

	receiver.AddDownTrack(downTrack, true)
	return nil
}

func (p *Publisher) relayReports(rp *relay.Peer) {
	for {
		time.Sleep(5 * time.Second)

		var r []rtcp.Packet
		for _, t := range rp.LocalTracks() {
			if dt, ok := t.(*DownTrack); ok {
				if !dt.bound.get() {
					continue
				}
				if sr := dt.CreateSenderReport(); sr != nil {
					r = append(r, sr)
				}
			}
		}

		if len(r) == 0 {
			continue
		}

		if err := rp.WriteRTCP(r); err != nil {
			if err == io.EOF || err == io.ErrClosedPipe {
				return
			}
			Logger.Error(err, "Sending downtrack reports err")
		}
	}
}
