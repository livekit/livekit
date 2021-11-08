package sfu

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/pion/webrtc/v3"
	"github.com/rs/zerolog/log"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/pion/ion-sfu/pkg/relay"
	"github.com/pion/ion-sfu/pkg/sfu/logger"
)

// Session represents a set of peers. Transports inside a SessionLocal
// are automatically subscribed to each other.
type Session interface {
	ID() string
	Publish(router Router, r Receiver)
	Subscribe(peer Peer)
	AddPeer(peer Peer)
	GetPeer(peerID string) Peer
	RemovePeer(peer Peer)
	AddRelayPeer(peerID string, signalData []byte) ([]byte, error)
	AudioObserver() *AudioObserver
	AddDatachannel(owner string, dc *webrtc.DataChannel)
	GetDCMiddlewares() []*Datachannel
	GetFanOutDataChannelLabels() []string
	GetDataChannels(peerID, label string) (dcs []*webrtc.DataChannel)
	FanOutMessage(origin, label string, msg webrtc.DataChannelMessage)
	Peers() []Peer
	RelayPeers() []*RelayPeer
}

type SessionLocal struct {
	id             string
	mu             sync.RWMutex
	config         WebRTCTransportConfig
	peers          map[string]Peer
	relayPeers     map[string]*RelayPeer
	closed         atomicBool
	audioObs       *AudioObserver
	fanOutDCs      []string
	datachannels   []*Datachannel
	onCloseHandler func()
}

const (
	AudioLevelsMethod = "audioLevels"
)

// NewSession creates a new SessionLocal
func NewSession(id string, dcs []*Datachannel, cfg WebRTCTransportConfig) Session {
	s := &SessionLocal{
		id:           id,
		peers:        make(map[string]Peer),
		relayPeers:   make(map[string]*RelayPeer),
		datachannels: dcs,
		config:       cfg,
		audioObs:     NewAudioObserver(cfg.Router.AudioLevelThreshold, cfg.Router.AudioLevelInterval, cfg.Router.AudioLevelFilter),
	}
	go s.audioLevelObserver(cfg.Router.AudioLevelInterval)
	return s
}

// ID return SessionLocal id
func (s *SessionLocal) ID() string {
	return s.id
}

func (s *SessionLocal) AudioObserver() *AudioObserver {
	return s.audioObs
}

func (s *SessionLocal) GetDCMiddlewares() []*Datachannel {
	return s.datachannels
}

func (s *SessionLocal) GetFanOutDataChannelLabels() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	fanout := make([]string, len(s.fanOutDCs))
	copy(fanout, s.fanOutDCs)
	return fanout
}

func (s *SessionLocal) AddPeer(peer Peer) {
	s.mu.Lock()
	s.peers[peer.ID()] = peer
	s.mu.Unlock()
}

func (s *SessionLocal) GetPeer(peerID string) Peer {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.peers[peerID]
}

func (s *SessionLocal) AddRelayPeer(peerID string, signalData []byte) ([]byte, error) {
	p, err := relay.NewPeer(relay.PeerMeta{
		PeerID:    peerID,
		SessionID: s.id,
	}, &relay.PeerConfig{
		SettingEngine: s.config.Setting,
		ICEServers:    s.config.Configuration.ICEServers,
		Logger:        logger.New(),
	})
	if err != nil {
		log.Err(err).Msg("Creating relay peer")
		return nil, status.Error(codes.Internal, err.Error())
	}

	resp, err := p.Answer(signalData)
	if err != nil {
		log.Err(err).Msg("Creating answer for relay")
		return nil, err
	}

	p.OnReady(func() {
		rp := NewRelayPeer(p, s, &s.config)
		s.mu.Lock()
		s.relayPeers[peerID] = rp
		s.mu.Unlock()
	})

	p.OnClose(func() {
		s.mu.Lock()
		delete(s.relayPeers, peerID)
		s.mu.Unlock()
	})

	return resp, nil
}

func (s *SessionLocal) GetRelayPeer(peerID string) *RelayPeer {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.relayPeers[peerID]
}

// RemovePeer removes Peer from the SessionLocal
func (s *SessionLocal) RemovePeer(p Peer) {
	pid := p.ID()
	Logger.V(0).Info("RemovePeer from SessionLocal", "peer_id", pid, "session_id", s.id)
	s.mu.Lock()
	if s.peers[pid] == p {
		delete(s.peers, pid)
	}
	peerCount := len(s.peers)
	s.mu.Unlock()

	// Close SessionLocal if no peers
	if peerCount == 0 {
		s.Close()
	}
}

func (s *SessionLocal) AddDatachannel(owner string, dc *webrtc.DataChannel) {
	label := dc.Label()

	s.mu.Lock()
	for _, lbl := range s.fanOutDCs {
		if label == lbl {
			s.mu.Unlock()
			return
		}
	}
	s.fanOutDCs = append(s.fanOutDCs, label)
	peerOwner := s.peers[owner]
	s.mu.Unlock()
	peers := s.Peers()
	peerOwner.Subscriber().RegisterDatachannel(label, dc)

	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		s.FanOutMessage(owner, label, msg)
	})

	for _, p := range peers {
		peer := p
		if peer.ID() == owner || peer.Subscriber() == nil {
			continue
		}
		ndc, err := peer.Subscriber().AddDataChannel(label)

		if err != nil {
			Logger.Error(err, "error adding datachannel")
			continue
		}

		if peer.Publisher() != nil && peer.Publisher().Relayed() {
			peer.Publisher().AddRelayFanOutDataChannel(label)
		}

		pid := peer.ID()
		ndc.OnMessage(func(msg webrtc.DataChannelMessage) {
			s.FanOutMessage(pid, label, msg)

			if peer.Publisher().Relayed() {
				for _, rdc := range peer.Publisher().GetRelayedDataChannels(label) {
					if msg.IsString {
						if err = rdc.SendText(string(msg.Data)); err != nil {
							Logger.Error(err, "Sending dc message err")
						}
					} else {
						if err = rdc.Send(msg.Data); err != nil {
							Logger.Error(err, "Sending dc message err")
						}
					}
				}
			}
		})

		peer.Subscriber().negotiate()
	}
}

// Publish will add a Sender to all peers in current SessionLocal from given
// Receiver
func (s *SessionLocal) Publish(router Router, r Receiver) {
	for _, p := range s.Peers() {
		// Don't sub to self
		if router.ID() == p.ID() || p.Subscriber() == nil {
			continue
		}

		Logger.V(0).Info("Publishing track to peer", "peer_id", p.ID())

		if err := router.AddDownTracks(p.Subscriber(), r); err != nil {
			Logger.Error(err, "Error subscribing transport to Router")
			continue
		}
	}
}

// Subscribe will create a Sender for every other Receiver in the SessionLocal
func (s *SessionLocal) Subscribe(peer Peer) {
	s.mu.RLock()
	fdc := make([]string, len(s.fanOutDCs))
	copy(fdc, s.fanOutDCs)
	peers := make([]Peer, 0, len(s.peers))
	for _, p := range s.peers {
		if p == peer || p.Publisher() == nil {
			continue
		}
		peers = append(peers, p)
	}
	s.mu.RUnlock()

	// Subscribe to fan out data channels
	for _, label := range fdc {
		dc, err := peer.Subscriber().AddDataChannel(label)
		if err != nil {
			Logger.Error(err, "error adding datachannel")
			continue
		}
		l := label
		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			s.FanOutMessage(peer.ID(), l, msg)

			if peer.Publisher().Relayed() {
				for _, rdc := range peer.Publisher().GetRelayedDataChannels(l) {
					if msg.IsString {
						if err = rdc.SendText(string(msg.Data)); err != nil {
							Logger.Error(err, "Sending dc message err")
						}
					} else {
						if err = rdc.Send(msg.Data); err != nil {
							Logger.Error(err, "Sending dc message err")
						}
					}

				}
			}
		})
	}

	// Subscribe to publisher streams
	for _, p := range peers {
		err := p.Publisher().GetRouter().AddDownTracks(peer.Subscriber(), nil)
		if err != nil {
			Logger.Error(err, "Subscribing to Router err")
			continue
		}
	}

	// Subscribe to relay streams
	for _, p := range s.RelayPeers() {
		err := p.GetRouter().AddDownTracks(peer.Subscriber(), nil)
		if err != nil {
			Logger.Error(err, "Subscribing to Router err")
			continue
		}
	}

	peer.Subscriber().negotiate()
}

// Peers returns peers in this SessionLocal
func (s *SessionLocal) Peers() []Peer {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p := make([]Peer, 0, len(s.peers))
	for _, peer := range s.peers {
		p = append(p, peer)
	}
	return p
}

// RelayPeers returns relay peers in this SessionLocal
func (s *SessionLocal) RelayPeers() []*RelayPeer {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p := make([]*RelayPeer, 0, len(s.peers))
	for _, peer := range s.relayPeers {
		p = append(p, peer)
	}
	return p
}

// OnClose is called when the SessionLocal is closed
func (s *SessionLocal) OnClose(f func()) {
	s.onCloseHandler = f
}

func (s *SessionLocal) Close() {
	if !s.closed.set(true) {
		return
	}
	if s.onCloseHandler != nil {
		s.onCloseHandler()
	}
}

func (s *SessionLocal) FanOutMessage(origin, label string, msg webrtc.DataChannelMessage) {
	dcs := s.GetDataChannels(origin, label)
	for _, dc := range dcs {
		if msg.IsString {
			if err := dc.SendText(string(msg.Data)); err != nil {
				Logger.Error(err, "Sending dc message err")
			}
		} else {
			if err := dc.Send(msg.Data); err != nil {
				Logger.Error(err, "Sending dc message err")
			}
		}
	}
}

func (s *SessionLocal) GetDataChannels(peerID, label string) []*webrtc.DataChannel {
	s.mu.RLock()
	defer s.mu.RUnlock()
	dcs := make([]*webrtc.DataChannel, 0, len(s.peers))
	for pid, p := range s.peers {
		if peerID == pid {
			continue
		}

		if p.Subscriber() != nil {
			if dc := p.Subscriber().DataChannel(label); dc != nil && dc.ReadyState() == webrtc.DataChannelStateOpen {
				dcs = append(dcs, dc)
			}
		}

	}
	for _, rp := range s.relayPeers {
		if dc := rp.DataChannel(label); dc != nil {
			dcs = append(dcs, dc)
		}
	}

	return dcs
}

func (s *SessionLocal) audioLevelObserver(audioLevelInterval int) {
	if audioLevelInterval <= 50 {
		Logger.V(0).Info("Values near/under 20ms may return unexpected values")
	}
	if audioLevelInterval == 0 {
		audioLevelInterval = 1000
	}
	for {
		time.Sleep(time.Duration(audioLevelInterval) * time.Millisecond)
		if s.closed.get() {
			return
		}
		levels := s.audioObs.Calc()

		if levels == nil {
			continue
		}

		msg := ChannelAPIMessage{
			Method: AudioLevelsMethod,
			Params: levels,
		}

		l, err := json.Marshal(&msg)
		if err != nil {
			Logger.Error(err, "Marshaling audio levels err")
			continue
		}

		sl := string(l)
		dcs := s.GetDataChannels("", APIChannelLabel)

		for _, ch := range dcs {
			if err = ch.SendText(sl); err != nil {
				Logger.Error(err, "Sending audio levels err")
			}
		}
	}
}
