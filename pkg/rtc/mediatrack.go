package rtc

import (
	"context"
	"io"
	"sync"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/rtcerr"

	"github.com/livekit/livekit-server/pkg/logger"
	"github.com/livekit/livekit-server/pkg/sfu"
	"github.com/livekit/livekit-server/pkg/utils"
	"github.com/livekit/livekit-server/proto/livekit"
)

var (
	maxPLIFrequency = 1 * time.Second
	feedbackTypes   = []webrtc.RTCPFeedback{{"goog-remb", ""}, {"nack", ""}, {"nack", "pli"}}
)

// MediaTrack represents a WebRTC track that needs to be forwarded
// Implements the PublishedTrack interface
type MediaTrack struct {
	ctx           context.Context
	id            string
	participantId string
	muted         bool
	// source remoteTrack
	remoteTrack *webrtc.TrackRemote
	// channel to send RTCP packets to the source
	rtcpCh chan []rtcp.Packet
	lock   sync.RWMutex
	once   sync.Once
	// map of target participantId -> forwarder
	forwarders map[string]Forwarder
	receiver   *Receiver
	//lastNack   int64
	lastPLI time.Time
}

func NewMediaTrack(pId string, rtcpCh chan []rtcp.Packet, track *webrtc.TrackRemote, receiver *Receiver) *MediaTrack {
	t := &MediaTrack{
		ctx:           context.Background(),
		id:            utils.NewGuid(utils.TrackPrefix),
		participantId: pId,
		remoteTrack:   track,
		rtcpCh:        rtcpCh,
		lock:          sync.RWMutex{},
		once:          sync.Once{},
		forwarders:    make(map[string]Forwarder),
		receiver:      receiver,
	}

	return t
}

func (t *MediaTrack) Start() {
	t.once.Do(func() {
		t.receiver.Start()
		// start worker
		go t.forwardRTPWorker()
	})
}

func (t *MediaTrack) ID() string {
	return t.id
}

func (t *MediaTrack) Kind() livekit.TrackInfo_Type {
	switch t.remoteTrack.Kind() {
	case webrtc.RTPCodecTypeVideo:
		return livekit.TrackInfo_VIDEO
	case webrtc.RTPCodecTypeAudio:
		return livekit.TrackInfo_AUDIO
	}
	panic("unsupported track kind")
}

func (t *MediaTrack) StreamID() string {
	return t.remoteTrack.StreamID()
}

func (t *MediaTrack) IsMuted() bool {
	return t.muted
}

// subscribes participant to current remoteTrack
// creates and add necessary forwarders and starts them
func (t *MediaTrack) AddSubscriber(participant Participant) error {
	codec := t.remoteTrack.Codec()
	// pack ID to identify all publishedTracks
	packedId := PackTrackId(t.participantId, t.id)

	// using DownTrack from ion-sfu
	outTrack, err := sfu.NewDownTrack(webrtc.RTPCodecCapability{
		MimeType:     codec.MimeType,
		ClockRate:    codec.ClockRate,
		Channels:     codec.Channels,
		SDPFmtpLine:  codec.SDPFmtpLine,
		RTCPFeedback: feedbackTypes,
	}, packedId, t.remoteTrack.StreamID())
	if err != nil {
		return err
	}

	transceiver, err := participant.PeerConnection().AddTransceiverFromTrack(outTrack, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionSendonly,
	})
	if err != nil {
		return err
	}

	outTrack.SetTransceiver(transceiver)
	// TODO: when outtrack is bound, start loop to send reports
	//outTrack.OnBind(func() {
	//	go sub.sendStreamDownTracksReports(recv.StreamID())
	//})
	participant.AddDownTrack(t.StreamID(), outTrack)

	forwarder := NewSimpleForwarder(t.ctx, t.rtcpCh, outTrack, t.receiver)
	forwarder.OnClose(func(f Forwarder) {
		t.lock.Lock()
		delete(t.forwarders, participant.ID())
		t.lock.Unlock()

		if participant.PeerConnection().ConnectionState() == webrtc.PeerConnectionStateClosed {
			return
		}
		sender := transceiver.Sender()
		if sender != nil {
			if err := participant.PeerConnection().RemoveTrack(sender); err != nil {
				if _, ok := err.(*rtcerr.InvalidStateError); !ok {
					logger.GetLogger().Warnw("could not remove remoteTrack from forwarder",
						"participant", participant.ID(),
						"err", err)
				}
			}
		}

		participant.RemoveDownTrack(t.StreamID(), outTrack)
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
func (t *MediaTrack) RemoveSubscriber(participantId string) {
	t.lock.RLock()
	defer t.lock.RUnlock()

	if forwarder := t.forwarders[participantId]; forwarder != nil {
		go forwarder.Close()
	}
}

func (t *MediaTrack) RemoveAllSubscribers() {
	logger.GetLogger().Debugw("removing all subscribers", "track", t.id)
	t.lock.RLock()
	defer t.lock.RUnlock()
	for _, f := range t.forwarders {
		go f.Close()
	}
	t.forwarders = make(map[string]Forwarder)
}

// forwardRTPWorker reads from the receiver and writes to each sender
func (t *MediaTrack) forwardRTPWorker() {
	defer func() {
		t.RemoveAllSubscribers()
		// TODO: send unpublished events?
	}()

	for {
		pkt, err := t.receiver.ReadRTP()
		if err == io.EOF {
			logger.GetLogger().Debugw("Track received EOF, closing",
				"participant", t.participantId,
				"track", t.id)
			return
		}

		if err != nil {
			logger.GetLogger().Errorw("error while reading RTP",
				"participant", t.participantId,
				"track", t.id)
		}

		if t.muted {
			// short circuit when track is muted
			continue
		}

		//logger.GetLogger().Debugw("read packet from remoteTrack",
		//	"participantId", t.participantId,
		//	"remoteTrack", t.remoteTrack.ID())
		t.lock.RLock()
		for dstId, forwarder := range t.forwarders {
			err := forwarder.WriteRTP(pkt)
			if err == io.EOF {
				// this participant unsubscribed, remove it
				t.RemoveSubscriber(dstId)
				continue
			}

			if err == sfu.ErrRequiresKeyFrame {
				delta := time.Now().Sub(t.lastPLI)
				if delta < maxPLIFrequency {
					continue
				}
				logger.GetLogger().Infow("keyframe required, sending PLI")
				rtcpPkts := []rtcp.Packet{
					&rtcp.PictureLossIndication{SenderSSRC: uint32(t.remoteTrack.SSRC()), MediaSSRC: pkt.SSRC},
				}
				// queue up a PLI, but don't block channel
				go func() {
					t.rtcpCh <- rtcpPkts
				}()
				t.lastPLI = time.Now()
			} else if err != nil {
				logger.GetLogger().Warnw("could not forward packet to participant",
					"src", t.participantId,
					"dest", dstId,
					"remoteTrack", t.id,
					"err", err)
			}
		}
		t.lock.RUnlock()
	}
}
