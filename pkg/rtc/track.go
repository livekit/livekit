package rtc

import (
	"sync"

	sfu "github.com/pion/ion-sfu/pkg"
	"github.com/pion/webrtc/v3"
)

/**
 * Peer track represents a track that needs to be forwarded
 */
type PeerTrack struct {
	id     string
	peerId string
	// source track
	track *webrtc.Track
	lock  sync.RWMutex
	// map of target peerId -> forwarder
	forwarders map[string]Forwarder
	receiver   sfu.Receiver
	lastNack   int64
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
