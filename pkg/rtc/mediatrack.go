package rtc

import (
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
	conf     ReceiverConfig
	onClose  func()

	// channel to send RTCP packets to the source
	rtcpCh chan []rtcp.Packet
	lock   sync.RWMutex
	// map of target participantId -> DownTrack
	downtracks map[string]*sfu.DownTrack
	twcc       *twcc.Responder
	receiver   sfu.Receiver
	//lastNack   int64
	lastPLI time.Time
}

func NewMediaTrack(trackId string, pId string, rtcpCh chan []rtcp.Packet, conf ReceiverConfig, track *webrtc.TrackRemote) *MediaTrack {
	t := &MediaTrack{
		id:            trackId,
		participantId: pId,
		ssrc:          track.SSRC(),
		streamID:      track.StreamID(),
		kind:          ToProtoTrackKind(track.Kind()),
		codec:         track.Codec(),
		conf:          conf,
		rtcpCh:        rtcpCh,
		lock:          sync.RWMutex{},
		downtracks:    make(map[string]*sfu.DownTrack),
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
	if !t.muted.TrySet(muted) {
		return
	}
	// mute all of the downtracks
	t.lock.RLock()
	for _, dt := range t.downtracks {
		dt.Mute(muted)
	}
	t.lock.RUnlock()
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

	// using DownTrack from ion-sfu
	downTrack, err := sfu.NewDownTrack(webrtc.RTPCodecCapability{
		MimeType:     codec.MimeType,
		ClockRate:    codec.ClockRate,
		Channels:     codec.Channels,
		SDPFmtpLine:  codec.SDPFmtpLine,
		RTCPFeedback: feedbackTypes,
	}, t.receiver, t.participantId)
	if err != nil {
		return err
	}

	transceiver, err := participant.PeerConnection().AddTransceiverFromTrack(downTrack, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionSendrecv,
	})
	if err != nil {
		return err
	}

	downTrack.SetTransceiver(transceiver)
	// when outtrack is bound, start loop to send reports
	downTrack.OnBind(func() {
		t.sendDownTrackBindingReports(participant.ID(), participant.RTCPChan())
	})
	downTrack.OnCloseHandler(func() {
		t.lock.Lock()
		delete(t.downtracks, participant.ID())
		t.lock.Unlock()

		// ignore if the subscribing participant is not connected
		if participant.PeerConnection().ConnectionState() == webrtc.PeerConnectionStateClosed {
			return
		}

		// if the source has been terminated, we'll need to terminate all of the downtracks
		// however, if the dest participant has disconnected, then we can skip
		sender := transceiver.Sender()
		if sender == nil {
			return
		}
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

		participant.RemoveDownTrack(t.streamID, downTrack)
	})

	t.lock.Lock()
	downTrack.Mute(t.muted.Get())
	t.downtracks[participant.ID()] = downTrack
	t.lock.Unlock()

	participant.AddDownTrack(t.streamID, downTrack)
	t.receiver.AddDownTrack(downTrack, true)

	return nil
}

// adds a new RTP receiver to the track, returns true if this is a new track
func (t *MediaTrack) AddReceiver(receiver *webrtc.RTPReceiver, track *webrtc.TrackRemote) {
	//rid := track.RID()

	buff, rtcpReader := bufferFactory.GetBufferPair(uint32(track.SSRC()))
	buff.OnFeedback(func(fb []rtcp.Packet) {
		t.rtcpCh <- fb
	})

	if t.Kind() == livekit.TrackType_AUDIO {
		// TODO: audio level stuff
	} else if t.Kind() == livekit.TrackType_VIDEO {
		if t.twcc == nil {
			t.twcc = twcc.NewTransportWideCCResponder(uint32(track.SSRC()))
			t.twcc.OnFeedback(func(p rtcp.RawPacket) {
				t.rtcpCh <- []rtcp.Packet{&p}
			})
		}
		buff.OnTransportWideCC(func(sn uint16, timeNS int64, marker bool) {
			t.twcc.Push(sn, timeNS, marker)
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

	if t.receiver == nil {
		// pack ID to identify all publishedTracks
		packedId := PackTrackId(t.participantId, track.ID())
		t.lock.Lock()
		t.receiver = NewWrappedReceiver(sfu.NewWebRTCReceiver(receiver, track, t.participantId), packedId)
		t.lock.Unlock()
		t.receiver.SetRTCPCh(t.rtcpCh)
		t.receiver.OnCloseHandler(func() {
			// source track closed
			if t.Kind() == livekit.TrackType_AUDIO {
				// TODO: remove audio level observer
			}
			t.lock.Lock()
			t.receiver = nil
			t.lock.Unlock()
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
	t.downtracks = make(map[string]*sfu.DownTrack)
}

func (t *MediaTrack) sendDownTrackBindingReports(participantId string, rtcpCh chan []rtcp.Packet) {
	var sd []rtcp.SourceDescriptionChunk

	t.lock.RLock()
	dt := t.downtracks[participantId]
	t.lock.RUnlock()

	chunks := dt.CreateSourceDescriptionChunks()
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
