package rtc

import (
	"context"
	"io"
	"sync"
	"time"

	"github.com/gammazero/workerpool"
	"github.com/pion/ion-sfu/pkg/buffer"
	"github.com/pion/rtcp"
	"github.com/pion/transport/packetio"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/rtcerr"

	"github.com/livekit/livekit-server/pkg/logger"
	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/sfu"
	"github.com/livekit/livekit-server/proto/livekit"
)

var (
	maxPLIFrequency = 1 * time.Second
	feedbackTypes   = []webrtc.RTCPFeedback{
		{webrtc.TypeRTCPFBGoogREMB, ""},
		{webrtc.TypeRTCPFBTransportCC, ""},
		{webrtc.TypeRTCPFBNACK, ""},
		{webrtc.TypeRTCPFBNACK, "pli"}}
)

// MediaTrack represents a WebRTC track that needs to be forwarded
// Implements the PublishedTrack interface
type MediaTrack struct {
	ctx           context.Context
	id            string
	participantId string
	muted         bool

	ssrc  webrtc.SSRC
	name  string
	kind  livekit.TrackType
	codec webrtc.RTPCodecParameters

	// channel to send RTCP packets to the source
	rtcpCh chan []rtcp.Packet
	lock   sync.RWMutex
	once   sync.Once
	// map of target participantId -> DownTrack
	downtracks map[string]types.DownTrack
	receiver   types.Receiver
	nackWorker *workerpool.WorkerPool
	//lastNack   int64
	lastPLI time.Time
}

func NewMediaTrack(trackId string, pId string, rtcpCh chan []rtcp.Packet, track *webrtc.TrackRemote, receiver types.Receiver) *MediaTrack {
	t := &MediaTrack{
		ctx:           context.Background(),
		id:            trackId,
		participantId: pId,
		ssrc:          track.SSRC(),
		name:          track.StreamID(),
		kind:          ToProtoTrackKind(track.Kind()),
		codec:         track.Codec(),
		rtcpCh:        rtcpCh,
		lock:          sync.RWMutex{},
		once:          sync.Once{},
		downtracks:    make(map[string]types.DownTrack),
		receiver:      receiver,
		nackWorker:    workerpool.New(1),
	}

	return t
}

func (t *MediaTrack) Start() {
	t.once.Do(func() {
		// start worker
		go t.forwardRTPWorker()
	})
}

func (t *MediaTrack) ID() string {
	return t.id
}

func (t *MediaTrack) Kind() livekit.TrackType {
	return t.kind
}

func (t *MediaTrack) Name() string {
	return t.name
}

func (t *MediaTrack) SetName(name string) {
	t.name = name
}

func (t *MediaTrack) IsMuted() bool {
	return t.muted
}

// subscribes participant to current remoteTrack
// creates and add necessary forwarders and starts them
func (t *MediaTrack) AddSubscriber(participant types.Participant) error {
	codec := t.codec
	// pack ID to identify all publishedTracks
	packedId := PackTrackId(t.participantId, t.id)

	// using DownTrack from ion-sfu
	outTrack, err := sfu.NewDownTrack(webrtc.RTPCodecCapability{
		MimeType:     codec.MimeType,
		ClockRate:    codec.ClockRate,
		Channels:     codec.Channels,
		SDPFmtpLine:  codec.SDPFmtpLine,
		RTCPFeedback: feedbackTypes,
	}, packedId, t.Name())
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
	outTrack.OnBind(func() {
		if rr := bufferFactory.GetOrNew(packetio.RTCPBufferPacket, outTrack.SSRC()).(*buffer.RTCPReader); rr != nil {
			rr.OnPacket(func(pkt []byte) {
				t.handleRTCP(outTrack, pkt)
			})
		}
		//go t.scheduleDownTrackBindingReports(recv.Name())
	})
	outTrack.OnCloseHandler(func() {
		t.lock.Lock()
		delete(t.downtracks, participant.ID())
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

		participant.RemoveDownTrack(t.Name(), outTrack)
	})
	participant.AddDownTrack(t.Name(), outTrack)

	t.lock.Lock()
	defer t.lock.Unlock()
	t.downtracks[participant.ID()] = outTrack

	return nil
}

// removes peer from subscription
// stop all forwarders to the peer
func (t *MediaTrack) RemoveSubscriber(participantId string) {
	t.lock.RLock()
	defer t.lock.RUnlock()

	if dt := t.downtracks[participantId]; dt != nil {
		go dt.Close()
	}
}

func (t *MediaTrack) RemoveAllSubscribers() {
	logger.GetLogger().Debugw("removing all subscribers", "track", t.id)
	t.lock.RLock()
	defer t.lock.RUnlock()
	for _, dt := range t.downtracks {
		go dt.Close()
	}
	t.downtracks = make(map[string]types.DownTrack)
}

//func (t *MediaTrack) scheduleDownTrackBindingReports(streamId string) {
//	var sd []rtcp.SourceDescriptionChunk
//
//	p.lock.RLock()
//	dts := p.subscribedTracks[streamId]
//	for _, dt := range dts {
//		if !dt.IsBound() {
//			continue
//		}
//		chunks := dt.CreateSourceDescriptionChunks()
//		if chunks != nil {
//			sd = append(sd, chunks...)
//		}
//	}
//	p.lock.RUnlock()
//
//	pkts := []rtcp.Packet{
//		&rtcp.SourceDescription{Chunks: sd},
//	}
//
//	go func() {
//		batch := pkts
//		i := 0
//		for {
//			if err := p.peerConn.WriteRTCP(batch); err != nil {
//				logger.GetLogger().Debugw("error sending track binding reports",
//					"participant", p.id,
//					"err", err)
//			}
//			if i > 5 {
//				return
//			}
//			i++
//			time.Sleep(20 * time.Millisecond)
//		}
//	}()
//}

// b reads from the receiver and writes to each sender
func (t *MediaTrack) forwardRTPWorker() {
	defer func() {
		logger.GetLogger().Debugw("stopping forward RTP worker")
		t.RemoveAllSubscribers()
		// TODO: send unpublished events?
		t.nackWorker.Stop()
	}()

	for pkt := range t.receiver.RTPChan() {
		//logger.GetLogger().Debugw("read packet from remoteTrack",
		//	"participant", t.participantId,
		//	"track", t.ID())
		// when track is muted, it's "disabled" on the client side, and will still be sending black frames
		// when our metadata is updated as such, we shortcircuit forwarding of black frames
		if t.muted {
			continue
		}

		t.lock.RLock()
		for dstId, dt := range t.downtracks {
			//logger.GetLogger().Debugw("read packet from remoteTrack",
			//	"srcParticipant", t.participantId,
			//	"destParticipant", dstId,
			//	"track", t.ID())
			err := dt.WriteRTP(pkt)
			if IsEOF(err) {
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
					&rtcp.PictureLossIndication{SenderSSRC: uint32(t.ssrc), MediaSSRC: pkt.SSRC},
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

func (t *MediaTrack) handleRTCP(dt *sfu.DownTrack, rtcpBuf []byte) {
	pkts, err := rtcp.Unmarshal(rtcpBuf)
	if err != nil {
		logger.GetLogger().Warnw("could not decode RTCP packet", "err", err)
	}

	var fwdPkts []rtcp.Packet
	// sufficient to send this only once
	pliOnce := true
	firOnce := true
	for _, pkt := range pkts {
		switch p := pkt.(type) {
		case *rtcp.PictureLossIndication:
			if pliOnce {
				p.MediaSSRC = dt.LastSSRC()
				p.SenderSSRC = dt.SSRC()
				fwdPkts = append(fwdPkts, p)
				pliOnce = false
			}
		case *rtcp.FullIntraRequest:
			if firOnce {
				p.MediaSSRC = dt.LastSSRC()
				p.SenderSSRC = dt.SSRC()
				fwdPkts = append(fwdPkts, p)
				firOnce = false
			}
		case *rtcp.ReceiverReport:
			if len(p.Reports) > 0 && p.Reports[0].FractionLost > 25 {
				//log.Tracef("Slow link for sender %s, fraction packet lost %.2f", f.track.peerID, float64(p.Reports[0].FractionLost)/256)
			}
		case *rtcp.TransportLayerNack:
			logger.GetLogger().Debugw("forwarder got nack",
				"packet", p)
			var nackedPackets []uint16
			for _, pair := range p.Nacks {
				nackedPackets = append(nackedPackets, dt.GetNACKSeqNo(pair.PacketList())...)
			}
			t.nackWorker.Submit(func() {
				pktBuf := packetFactory.Get().([]byte)
				for _, sn := range nackedPackets {
					pkt, err := t.receiver.GetBufferedPacket(pktBuf, sn, dt.SnOffset())
					if err == io.EOF {
						break
					} else if err != nil {
						continue
					}
					// what about handling write errors such as no packet found
					dt.WriteRTP(pkt)
				}
				packetFactory.Put(pktBuf)
			})
		}
	}

	if len(fwdPkts) > 0 {
		t.rtcpCh <- fwdPkts
	}
}
