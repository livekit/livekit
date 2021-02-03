package rtc

import (
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
	"github.com/livekit/livekit-server/pkg/utils"
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
	id            string
	participantId string
	muted         utils.AtomicFlag

	ssrc     webrtc.SSRC
	name     string
	streamID string
	kind     livekit.TrackType
	codec    webrtc.RTPCodecParameters
	onClose  func()

	// channel to send RTCP packets to the source
	rtcpCh *utils.CalmChannel
	lock   sync.RWMutex
	once   sync.Once
	// map of target participantId -> DownTrack
	downtracks map[string]types.DownTrack
	receiver   types.Receiver
	nackWorker *workerpool.WorkerPool
	//lastNack   int64
	lastPLI time.Time
}

func NewMediaTrack(trackId string, pId string, rtcpCh *utils.CalmChannel, track *webrtc.TrackRemote, receiver types.Receiver) *MediaTrack {
	t := &MediaTrack{
		id:            trackId,
		participantId: pId,
		ssrc:          track.SSRC(),
		streamID:      track.StreamID(),
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

func (t *MediaTrack) IsMuted() bool {
	return t.muted.Get()
}

func (t *MediaTrack) OnClose(f func()) {
	t.onClose = f
}

// subscribes participant to current remoteTrack
// creates and add necessary forwarders and starts them
func (t *MediaTrack) AddSubscriber(participant types.Participant) error {
	t.lock.RLock()
	existingDt := t.downtracks[participant.ID()]
	t.lock.RUnlock()

	// don't subscribe to the same track multiple times
	if existingDt != nil {
		logger.Warnw("participant already subscribed to track",
			"participant", participant.Identity(),
			"track", existingDt.ID())
		return nil
	}

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
	}, packedId, t.streamID)
	if err != nil {
		return err
	}

	transceiver, err := participant.PeerConnection().AddTransceiverFromTrack(outTrack, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionSendrecv,
	})
	if err != nil {
		return err
	}

	outTrack.SetTransceiver(transceiver)
	// when outtrack is bound, start loop to send reports
	outTrack.OnBind(func() {
		if rr := bufferFactory.GetOrNew(packetio.RTCPBufferPacket, outTrack.SSRC()).(*buffer.RTCPReader); rr != nil {
			rr.OnPacket(func(pkt []byte) {
				t.handleRTCP(outTrack, participant.Identity(), pkt)
			})
		}
		t.sendDownTrackBindingReports(participant.ID(), participant.RTCPChan())
	})
	outTrack.OnCloseHandler(func() {
		t.lock.Lock()
		delete(t.downtracks, participant.ID())
		t.lock.Unlock()

		// ignore if the subscribing participant is not connected
		if participant.PeerConnection().ConnectionState() != webrtc.PeerConnectionStateConnected {
			return
		}

		// if the source has been terminated, we'll need to terminate all of the downtracks
		// however, if the dest participant has disconnected, then we can skip
		sender := transceiver.Sender()
		if sender != nil {
			logger.Debugw("removing peerconnection track",
				"track", t.id,
				"participantId", t.participantId,
				"destParticipant", participant.Identity())
			if err := participant.PeerConnection().RemoveTrack(sender); err != nil {
				if err == webrtc.ErrConnectionClosed {
					// participant closing, can skip removing downtracks
					return
				}
				if _, ok := err.(*rtcerr.InvalidStateError); !ok {
					logger.Warnw("could not remove remoteTrack from forwarder",
						"participant", participant.Identity(),
						"err", err)
				}
			}
		}

		participant.RemoveDownTrack(t.streamID, outTrack)
	})
	participant.AddDownTrack(t.streamID, outTrack)

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
	logger.Debugw("removing all subscribers", "track", t.id)
	t.lock.RLock()
	defer t.lock.RUnlock()
	for _, dt := range t.downtracks {
		go dt.Close()
	}
	t.downtracks = make(map[string]types.DownTrack)
}

func (t *MediaTrack) sendDownTrackBindingReports(participantId string, rtcpCh *utils.CalmChannel) {
	var sd []rtcp.SourceDescriptionChunk

	t.lock.RLock()
	dt := t.downtracks[participantId]
	t.lock.RUnlock()
	if !dt.IsBound() {
		return
	}
	chunks := dt.CreateSourceDescriptionChunks()
	if chunks != nil {
		sd = append(sd, chunks...)
	}

	pkts := []rtcp.Packet{
		&rtcp.SourceDescription{Chunks: sd},
	}

	go func() {
		defer RecoverSilent()
		batch := pkts
		i := 0
		for {
			rtcpCh.Write(batch)
			if i > 5 {
				return
			}
			i++
			time.Sleep(20 * time.Millisecond)
		}
	}()
}

// b reads from the receiver and writes to each sender
func (t *MediaTrack) forwardRTPWorker() {
	defer Recover()
	defer func() {
		t.RemoveAllSubscribers()
		// TODO: send unpublished events?
		t.nackWorker.Stop()

		if t.onClose != nil {
			t.onClose()
		}
	}()

	for pkt := range t.receiver.RTPChan() {
		//logger.Debugw("read packet from remoteTrack",
		//	"participant", t.participantId,
		//	"track", t.ID())
		// when track is muted, it's "disabled" on the client side, and will still be sending black frames
		// when our metadata is updated as such, we shortcircuit forwarding of black frames
		if t.muted.Get() {
			continue
		}

		t.lock.RLock()
		for dstId, dt := range t.downtracks {
			//logger.Debugw("read packet from remoteTrack",
			//	"srcParticipant", t.participantId,
			//	"destParticipant", dstId,
			//	"track", t.ID())
			err := dt.WriteRTP(pkt.Packet)
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
				logger.Infow("keyframe required, sending PLI",
					"srcParticipant", t.participantId)
				rtcpPkts := []rtcp.Packet{
					&rtcp.PictureLossIndication{SenderSSRC: uint32(t.ssrc), MediaSSRC: pkt.Packet.SSRC},
				}
				t.rtcpCh.Write(rtcpPkts)
				t.lastPLI = time.Now()
			} else if err != nil {
				logger.Warnw("could not forward packet to participant",
					"src", t.participantId,
					"dest", dstId,
					"remoteTrack", t.id,
					"err", err)
			}
		}
		t.lock.RUnlock()
	}
}

func (t *MediaTrack) handleRTCP(dt *sfu.DownTrack, identity string, rtcpBuf []byte) {
	defer Recover()
	pkts, err := rtcp.Unmarshal(rtcpBuf)
	if err != nil {
		logger.Warnw("could not decode RTCP packet", "err", err)
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
			logger.Debugw("forwarder got nack",
				"participant", identity,
				"packet", p)
			var nackedPackets []uint16
			for _, pair := range p.Nacks {
				nackedPackets = append(nackedPackets, dt.GetNACKSeqNo(pair.PacketList())...)
			}

			if t.nackWorker.Stopped() {
				return
			}
			t.nackWorker.Submit(func() {
				pktBuf := packetFactory.Get().([]byte)
				for _, sn := range nackedPackets {
					pkt, err := t.receiver.GetBufferedPacket(pktBuf, sn, dt.SnOffset())
					if err == io.EOF {
						break
					} else if err != nil {
						logger.Warnw("error getting buffered packet", "error", err)
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
		t.rtcpCh.Write(fwdPkts)
	}
}
