package rtc

import (
	"context"
	"sync"
	"time"

	"github.com/pion/webrtc/v3"

	"github.com/livekit/livekit-server/pkg/logger"
)

var (
	creationDelay = 500 * time.Millisecond
)

// Peer track represents a track that needs to be forwarded
type PeerTrack struct {
	id     string
	ctx    context.Context
	peerId string
	// source track
	track *webrtc.Track
	// source RTCPWriter to forward RTCP requests
	rtcpWriter RTCPWriter
	lock       sync.RWMutex
	// map of target peerId -> forwarder
	forwarders map[string]Forwarder
	receiver   *Receiver
	lastNack   int64
}

func NewPeerTrack(ctx context.Context, peerId string, rtcpWriter RTCPWriter, track *webrtc.Track, receiver *Receiver) *PeerTrack {
	return &PeerTrack{
		id:         track.ID(),
		ctx:        ctx,
		peerId:     peerId,
		track:      track,
		rtcpWriter: rtcpWriter,
		lock:       sync.RWMutex{},
		forwarders: make(map[string]Forwarder),
		receiver:   receiver,
	}
}

func (t *PeerTrack) Start() {
	t.receiver.Start()
	// start worker
	go t.forwardWorker()
}

// subscribes peer to current track
// creates and add necessary forwarders and starts them
func (t *PeerTrack) AddSubscriber(peer *WebRTCPeer) error {
	// check codecs supported by outbound peer
	codecs := peer.mediaEngine.GetCodecsByName(t.track.Codec().Name)
	if len(codecs) == 0 {
		return ErrUnsupportedPayloadType
	}

	// use existing SSRC with simple forwarders. adaptive forwarders require unique SSRC per layer
	outTrack, err := peer.conn.NewTrack(codecs[0].PayloadType, t.track.SSRC(), t.track.ID(), t.track.Label())
	if err != nil {
		return err
	}

	rtpSender, err := peer.conn.AddTrack(outTrack)
	if err != nil {
		return err
	}

	forwarder := NewSimpleForwarder(t.ctx, t.rtcpWriter, rtpSender, t.receiver.buffer)
	forwarder.OnClose(func(f Forwarder) {
		t.lock.Lock()
		delete(t.forwarders, peer.ID())
		t.lock.Unlock()

		if err := peer.conn.RemoveTrack(rtpSender); err != nil {
			logger.GetLogger().Warnw("could not remove track from forwarder",
				"peer", peer.ID(),
				"err", err)
		}
	})

	t.lock.Lock()
	defer t.lock.Unlock()
	t.forwarders[peer.ID()] = forwarder

	// start forwarder
	forwarder.Start()

	return nil
}

// removes peer from subscription
// stop all forwarders to the peer
func (t *PeerTrack) RemoveSubscriber(peerId string) {
	t.lock.RLock()
	defer t.lock.RUnlock()

	if forwarder := t.forwarders[peerId]; forwarder != nil {
		go forwarder.Close()
	}
}

// forwardWorker reads from the receiver and writes to each sender
func (t *PeerTrack) forwardWorker() {
	for pkt := range t.receiver.RTPChan() {
		if t.ctx.Err() != nil {
			return
		}
		now := time.Now()

		//logger.GetLogger().Debugw("read packet from track",
		//	"peerId", t.peerId,
		//	"track", t.track.ID())
		t.lock.RLock()
		for dstPeerId, forwarder := range t.forwarders {
			// There exists a bug in chrome where setLocalDescription
			// fails if track RTP arrives before the sfu offer is set.
			// We delay sending RTP here to avoid the issue.
			// https://bugs.chromium.org/p/webrtc/issues/detail?id=10139
			if now.Sub(forwarder.CreatedAt()) < creationDelay {
				continue
			}
			if err := forwarder.WriteRTP(pkt); err != nil {
				logger.GetLogger().Warnw("could not forward packet to peer",
					"srcPeer", t.peerId,
					"dstPeer", dstPeerId,
					"track", t.track.SSRC(),
					"err", err)
			}
		}
		t.lock.RUnlock()
	}
}
