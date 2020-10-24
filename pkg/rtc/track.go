package rtc

import (
	"sync"

	"github.com/pion/webrtc/v3"
)

/**
 * Peer track represents a track that needs to be forwarded
 */
type PeerTrack struct {
	id     uint32
	peerId string
	// source track
	track *webrtc.Track
	lock  sync.RWMutex
	// map of target peerId -> forwarder
	forwarders map[string]Forwarder
	receiver   Receiver
	lastNack   int64
}

func NewPeerTrack(peerId string, track *webrtc.Track, receiver Receiver) *PeerTrack {
	return &PeerTrack{
		id:         track.SSRC(),
		peerId:     peerId,
		track:      track,
		lock:       sync.RWMutex{},
		forwarders: make(map[string]Forwarder),
		receiver:   receiver,
	}
}

// subscribes peer to current track
// creates and add necessary forwarders and starts them
func (t *PeerTrack) AddSubscriber(peer *WebRTCPeer) error {
	return nil
}

// removes peer from subscription
// stop all forwarders to the peer
func (t *PeerTrack) RemoveSubscriber(peerId string) error {
	return nil
}
