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

// Track represents a mediaTrack that needs to be forwarded
type Track struct {
	id            string
	ctx           context.Context
	participantId string
	// source mediaTrack
	mediaTrack *webrtc.Track
	// source RTCPWriter to forward RTCP requests
	rtcpWriter RTCPWriter
	lock       sync.RWMutex
	// map of target participantId -> forwarder
	forwarders map[string]Forwarder
	receiver   *Receiver
	lastNack   int64
}

func NewTrack(ctx context.Context, pId string, rtcpWriter RTCPWriter, mediaTrack *webrtc.Track, receiver *Receiver) *Track {
	return &Track{
		id:            mediaTrack.ID(),
		ctx:           ctx,
		participantId: pId,
		mediaTrack:    mediaTrack,
		rtcpWriter:    rtcpWriter,
		lock:          sync.RWMutex{},
		forwarders:    make(map[string]Forwarder),
		receiver:      receiver,
	}
}

func (t *Track) Start() {
	t.receiver.Start()
	// start worker
	go t.forwardWorker()
}

// subscribes participant to current mediaTrack
// creates and add necessary forwarders and starts them
func (t *Track) AddSubscriber(participant *Participant) error {
	// check codecs supported by outbound participant
	codecs := participant.mediaEngine.GetCodecsByName(t.mediaTrack.Codec().Name)
	if len(codecs) == 0 {
		return ErrUnsupportedPayloadType
	}

	// pack ID to identify all tracks
	packedId := PackTrackId(t.participantId, t.mediaTrack.ID())

	// use existing SSRC with simple forwarders. adaptive forwarders require unique SSRC per layer
	outTrack, err := participant.peerConn.NewTrack(codecs[0].PayloadType, t.mediaTrack.SSRC(), packedId, t.mediaTrack.Label())
	if err != nil {
		return err
	}

	rtpSender, err := participant.peerConn.AddTrack(outTrack)
	if err != nil {
		return err
	}

	forwarder := NewSimpleForwarder(t.ctx, t.rtcpWriter, rtpSender, t.receiver.buffer)
	forwarder.OnClose(func(f Forwarder) {
		t.lock.Lock()
		delete(t.forwarders, participant.ID())
		t.lock.Unlock()

		if participant.peerConn.ConnectionState() == webrtc.PeerConnectionStateClosed {
			return
		}
		if err := participant.peerConn.RemoveTrack(rtpSender); err != nil {
			logger.GetLogger().Warnw("could not remove mediaTrack from forwarder",
				"participant", participant.ID(),
				"err", err)
		}
	})

	t.lock.Lock()
	defer t.lock.Unlock()
	t.forwarders[participant.ID()] = forwarder

	// start forwarder
	forwarder.Start()

	return nil
}

// removes peer from subscription
// stop all forwarders to the peer
func (t *Track) RemoveSubscriber(participantId string) {
	t.lock.RLock()
	defer t.lock.RUnlock()

	if forwarder := t.forwarders[participantId]; forwarder != nil {
		go forwarder.Close()
	}
}

// forwardWorker reads from the receiver and writes to each sender
func (t *Track) forwardWorker() {
	for pkt := range t.receiver.RTPChan() {
		if t.ctx.Err() != nil {
			return
		}
		now := time.Now()

		//logger.GetLogger().Debugw("read packet from mediaTrack",
		//	"participantId", t.participantId,
		//	"mediaTrack", t.mediaTrack.ID())
		t.lock.RLock()
		for dstId, forwarder := range t.forwarders {
			// There exists a bug in chrome where setLocalDescription
			// fails if mediaTrack RTP arrives before the sfu offer is set.
			// We refrain from sending RTP here to avoid the issue.
			// https://bugs.chromium.org/p/webrtc/issues/detail?id=10139
			if now.Sub(forwarder.CreatedAt()) < creationDelay {
				continue
			}
			if err := forwarder.WriteRTP(pkt); err != nil {
				logger.GetLogger().Warnw("could not forward packet to participant",
					"src", t.participantId,
					"dest", dstId,
					"mediaTrack", t.mediaTrack.SSRC(),
					"err", err)
			}
		}
		t.lock.RUnlock()
	}
}
