package rtc

import (
	"errors"
	"sync"
	"time"

	"github.com/pion/ion-sfu/pkg/buffer"
	"github.com/pion/ion-sfu/pkg/sfu"
	"github.com/pion/ion-sfu/pkg/twcc"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/rtcerr"

	"github.com/livekit/livekit-server/pkg/logger"
	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/utils"
	"github.com/livekit/livekit-server/proto/livekit"
)

var (
	maxPLIFrequency = 1 * time.Second
	feedbackTypes   = []webrtc.RTCPFeedback{
		{webrtc.TypeRTCPFBGoogREMB, ""},
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
	conf     ReceiverConfig
	onClose  func()

	// channel to send RTCP packets to the source
	rtcpCh chan []rtcp.Packet
	lock   sync.RWMutex
	// map of target participantId -> *SubscribedTrack
	subscribedTracks map[string]*SubscribedTrack
	twcc             *twcc.Responder
	receiver         sfu.Receiver
	//lastNack   int64
	lastPLI time.Time
}

func NewMediaTrack(trackId string, pId string, rtcpCh chan []rtcp.Packet, conf ReceiverConfig, track *webrtc.TrackRemote) *MediaTrack {
	t := &MediaTrack{
		id:               trackId,
		participantId:    pId,
		ssrc:             track.SSRC(),
		streamID:         track.StreamID(),
		kind:             ToProtoTrackKind(track.Kind()),
		codec:            track.Codec(),
		conf:             conf,
		rtcpCh:           rtcpCh,
		lock:             sync.RWMutex{},
		subscribedTracks: make(map[string]*SubscribedTrack),
	}

	return t
}

func (t *MediaTrack) Start() {
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

func (t *MediaTrack) SetMuted(muted bool) {
	t.muted.TrySet(muted)

	// mute all of the subscribedtracks
	t.lock.RLock()
	for id, st := range t.subscribedTracks {
		logger.Debugw("setting muted", "dstParticipant", id, "muted", muted, "track", t.ID())
		st.SetPublisherMuted(muted)
	}
	t.lock.RUnlock()
}

func (t *MediaTrack) OnClose(f func()) {
	t.onClose = f
}

// subscribes participant to current remoteTrack
// creates and add necessary forwarders and starts them
func (t *MediaTrack) AddSubscriber(sub types.Participant) error {
	t.lock.Lock()
	defer t.lock.Unlock()
	existingSt := t.subscribedTracks[sub.ID()]

	// don't subscribe to the same track multiple times
	if existingSt != nil {
		logger.Warnw("participant already subscribed to track",
			"sub", sub.Identity(),
			"track", t.ID())
		return nil
	}

	if t.receiver == nil {
		// cannot add, no receiver
		return errors.New("cannot subscribe without a receiver in place")
	}

	codec := t.receiver.Codec()

	// using DownTrack from ion-sfu
	downTrack, err := sfu.NewDownTrack(webrtc.RTPCodecCapability{
		MimeType:     codec.MimeType,
		ClockRate:    codec.ClockRate,
		Channels:     codec.Channels,
		SDPFmtpLine:  codec.SDPFmtpLine,
		RTCPFeedback: feedbackTypes,
	}, t.receiver, sub.ID())
	if err != nil {
		return err
	}
	downTrack.SetBufferFactory(bufferFactory)
	subTrack := NewSubscribedTrack(downTrack)

	transceiver, err := sub.PeerConnection().AddTransceiverFromTrack(downTrack, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionSendrecv,
	})
	if err != nil {
		return err
	}

	downTrack.SetTransceiver(transceiver)
	// when outtrack is bound, start loop to send reports
	downTrack.OnBind(func() {
		subTrack.SetPublisherMuted(t.IsMuted())
		t.sendDownTrackBindingReports(sub.ID(), sub.RTCPChan())
	})
	downTrack.OnCloseHandler(func() {
		t.lock.Lock()
		delete(t.subscribedTracks, sub.ID())
		t.lock.Unlock()

		// ignore if the subscribing sub is not connected
		if sub.PeerConnection().ConnectionState() == webrtc.PeerConnectionStateClosed {
			return
		}

		// if the source has been terminated, we'll need to terminate all of the subscribedtracks
		// however, if the dest sub has disconnected, then we can skip
		sender := transceiver.Sender()
		if sender == nil {
			return
		}
		logger.Debugw("removing peerconnection track",
			"track", t.id,
			"participantId", t.participantId,
			"destParticipant", sub.Identity())
		if err := sub.PeerConnection().RemoveTrack(sender); err != nil {
			if err == webrtc.ErrConnectionClosed {
				// sub closing, can skip removing subscribedtracks
				return
			}
			if _, ok := err.(*rtcerr.InvalidStateError); !ok {
				logger.Warnw("could not remove remoteTrack from forwarder",
					"sub", sub.Identity(),
					"err", err)
			}
		}

		sub.RemoveSubscribedTrack(t.participantId, subTrack)
	})

	t.subscribedTracks[sub.ID()] = subTrack

	sub.AddSubscribedTrack(t.participantId, subTrack)
	t.receiver.AddDownTrack(downTrack, true)

	return nil
}

// adds a new RTP receiver to the track, returns true if this is a new track
func (t *MediaTrack) AddReceiver(receiver *webrtc.RTPReceiver, track *webrtc.TrackRemote) {
	//rid := track.RID()
	buff, rtcpReader := bufferFactory.GetBufferPair(uint32(track.SSRC()))
	buff.OnFeedback(func(fb []rtcp.Packet) {
		// feedback for the source RTCP
		t.rtcpCh <- fb
	})

	if t.Kind() == livekit.TrackType_AUDIO {
		// TODO: audio level stuff
	} else if t.Kind() == livekit.TrackType_VIDEO {
		// TODO: handle twcc
		//if t.twcc == nil {
		//	t.twcc = twcc.NewTransportWideCCResponder(uint32(track.SSRC()))
		//	t.twcc.OnFeedback(func(p rtcp.RawPacket) {
		//		t.rtcpCh <- []rtcp.Packet{&p}
		//	})
		//}
		buff.OnTransportWideCC(func(sn uint16, timeNS int64, marker bool) {
			//t.twcc.Push(sn, timeNS, marker)
		})
	}

	rtcpReader.OnPacket(func(bytes []byte) {
		pkts, err := rtcp.Unmarshal(bytes)
		if err != nil {
			logger.Errorw("could not unmarshal RTCP", "error", err)
			return
		}

		for _, pkt := range pkts {
			switch pkt := pkt.(type) {
			case *rtcp.SourceDescription:
			// do nothing for now
			case *rtcp.SenderReport:
				buff.SetSenderReportData(pkt.RTPTime, pkt.NTPTime)
			}
		}
	})

	t.lock.Lock()
	defer t.lock.Unlock()
	if t.receiver == nil {
		// pack ID to identify all publishedTracks
		packedId := PackTrackId(t.participantId, track.ID())
		t.receiver = NewWrappedReceiver(sfu.NewWebRTCReceiver(receiver, track, t.participantId), packedId)
		t.receiver.SetRTCPCh(t.rtcpCh)
		t.receiver.OnCloseHandler(func() {
			t.lock.Lock()
			// source track closed
			if t.Kind() == livekit.TrackType_AUDIO {
				// TODO: remove audio level observer
			}
			t.receiver = nil
			onclose := t.onClose
			t.lock.Unlock()
			t.RemoveAllSubscribers()
			if onclose != nil {
				onclose()
			}
		})
	}
	t.receiver.AddUpTrack(track, buff)

	buff.Bind(receiver.GetParameters(), buffer.Options{
		BufferTime: t.conf.maxBufferTime,
		MaxBitRate: t.conf.maxBitrate,
	})
}

// removes peer from subscription
// stop all forwarders to the peer
func (t *MediaTrack) RemoveSubscriber(participantId string) {
	t.lock.RLock()
	defer t.lock.RUnlock()

	if subTrack := t.subscribedTracks[participantId]; subTrack != nil {
		go subTrack.DownTrack().Close()
	}
}

func (t *MediaTrack) RemoveAllSubscribers() {
	logger.Debugw("removing all subscribers", "track", t.id)
	t.lock.RLock()
	defer t.lock.RUnlock()
	for _, subTrack := range t.subscribedTracks {
		go subTrack.DownTrack().Close()
	}
	t.subscribedTracks = make(map[string]*SubscribedTrack)
}

func (t *MediaTrack) sendDownTrackBindingReports(participantId string, rtcpCh chan []rtcp.Packet) {
	var sd []rtcp.SourceDescriptionChunk

	t.lock.RLock()
	subTrack := t.subscribedTracks[participantId]
	t.lock.RUnlock()

	if subTrack == nil {
		return
	}

	chunks := subTrack.DownTrack().CreateSourceDescriptionChunks()
	if chunks == nil {
		return
	}
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
			rtcpCh <- batch
			if i > 5 {
				return
			}
			i++
			time.Sleep(20 * time.Millisecond)
		}
	}()
}
