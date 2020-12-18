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
	creationDelay   = 500 * time.Millisecond
	maxPLIFrequency = 1 * time.Second
	feedbackTypes   = []webrtc.RTCPFeedback{{"goog-remb", ""}, {"nack", ""}, {"nack", "pli"}}
)

// Track represents a remoteTrack that needs to be forwarded
type Track struct {
	ctx           context.Context
	id            string
	participantId string
	// source remoteTrack
	remoteTrack *webrtc.TrackRemote
	// channel to send RTCP packets to the source
	rtcpCh chan []rtcp.Packet
	lock   sync.RWMutex
	// map of target participantId -> forwarder
	forwarders map[string]Forwarder
	receiver   *Receiver
	lastNack   int64
	lastPLI    time.Time
}

func NewTrack(pId string, rtcpCh chan []rtcp.Packet, track *webrtc.TrackRemote, receiver *Receiver) *Track {
	t := &Track{
		ctx:           context.Background(),
		id:            utils.NewGuid(utils.TrackPrefix),
		participantId: pId,
		remoteTrack:   track,
		rtcpCh:        rtcpCh,
		lock:          sync.RWMutex{},
		forwarders:    make(map[string]Forwarder),
		receiver:      receiver,
	}

	return t
}

func (t *Track) Start() {
	t.receiver.Start()
	// start worker
	go t.forwardRTPWorker()
}

func (t *Track) Kind() webrtc.RTPCodecType {
	return t.remoteTrack.Kind()
}

func (t *Track) StreamID() string {
	return t.remoteTrack.StreamID()
}

// subscribes participant to current remoteTrack
// creates and add necessary forwarders and starts them
func (t *Track) AddSubscriber(participant *Participant) error {
	codec := t.remoteTrack.Codec()
	// pack ID to identify all tracks
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

	transceiver, err := participant.peerConn.AddTransceiverFromTrack(outTrack, webrtc.RTPTransceiverInit{
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
	participant.addDownTrack(t.StreamID(), outTrack)

	forwarder := NewSimpleForwarder(t.ctx, t.rtcpCh, outTrack, t.receiver)
	forwarder.OnClose(func(f Forwarder) {
		t.lock.Lock()
		delete(t.forwarders, participant.ID())
		t.lock.Unlock()

		if participant.peerConn.ConnectionState() == webrtc.PeerConnectionStateClosed {
			return
		}
		sender := transceiver.Sender()
		if sender != nil {
			if err := participant.peerConn.RemoveTrack(sender); err != nil {
				if _, ok := err.(*rtcerr.InvalidStateError); !ok {
					logger.GetLogger().Warnw("could not remove remoteTrack from forwarder",
						"participant", participant.ID(),
						"err", err)
				}
			}
		}

		participant.removeDownTrack(t.StreamID(), outTrack)
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

func (t *Track) RemoveAllSubscribers() {
	logger.GetLogger().Debugw("removing all subscribers", "track", t.id)
	t.lock.RLock()
	defer t.lock.RUnlock()
	for _, f := range t.forwarders {
		go f.Close()
	}
}

func (t *Track) ToProto() *livekit.TrackInfo {
	var kind livekit.TrackInfo_Type
	switch t.Kind() {
	case webrtc.RTPCodecTypeAudio:
		kind = livekit.TrackInfo_AUDIO
	case webrtc.RTPCodecTypeVideo:
		kind = livekit.TrackInfo_VIDEO
	}

	return &livekit.TrackInfo{
		Sid:  t.id,
		Type: kind,
		Name: t.remoteTrack.StreamID(),
	}
}

// forwardRTPWorker reads from the receiver and writes to each sender
func (t *Track) forwardRTPWorker() {
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
