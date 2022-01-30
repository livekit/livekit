package rtc

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/livekit/livekit-server/pkg/sfu/connectionquality"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/sfu"
	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/sfu/twcc"
	"github.com/livekit/livekit-server/pkg/telemetry"
)

const (
	connectionQualityUpdateInterval = 5 * time.Second
)

// MediaTrack represents a WebRTC track that needs to be forwarded
// Implements MediaTrack and PublishedTrack interface
type MediaTrack struct {
	params      MediaTrackParams
	numUpTracks uint32
	buffer      *buffer.Buffer

	layerSSRCs [livekit.VideoQuality_HIGH + 1]uint32

	audioLevelMu sync.RWMutex
	audioLevel   *AudioLevel

	connectionStats *connectionquality.ConnectionStats

	done chan struct{}

	*MediaTrackReceiver

	lock sync.RWMutex
}

type MediaTrackParams struct {
	TrackInfo           *livekit.TrackInfo
	SignalCid           string
	SdpCid              string
	ParticipantID       livekit.ParticipantID
	ParticipantIdentity livekit.ParticipantIdentity
	// channel to send RTCP packets to the source
	RTCPChan         chan []rtcp.Packet
	BufferFactory    *buffer.Factory
	ReceiverConfig   ReceiverConfig
	SubscriberConfig DirectionConfig
	AudioConfig      config.AudioConfig
	Telemetry        telemetry.TelemetryService
	Logger           logger.Logger
}

func NewMediaTrack(track *webrtc.TrackRemote, params MediaTrackParams) *MediaTrack {
	t := &MediaTrack{
		params: params,
	}

	t.MediaTrackReceiver = NewMediaTrackReceiver(MediaTrackReceiverParams{
		TrackInfo:           params.TrackInfo,
		MediaTrack:          t,
		ParticipantID:       params.ParticipantID,
		ParticipantIdentity: params.ParticipantIdentity,
		BufferFactory:       params.BufferFactory,
		ReceiverConfig:      params.ReceiverConfig,
		SubscriberConfig:    params.SubscriberConfig,
		Telemetry:           params.Telemetry,
		Logger:              params.Logger,
	})
	t.MediaTrackReceiver.OnMediaLossUpdate(func(fractionalLoss uint8) {
		if t.buffer != nil && t.Kind() == livekit.TrackType_AUDIO {
			// ok to access buffer since receivers are added before subscribers
			t.buffer.SetLastFractionLostReport(fractionalLoss)
		}
	})
	t.MediaTrackReceiver.OnVideoLayerUpdate(func(layers []*livekit.VideoLayer) {
		for _, layer := range layers {
			t.params.Telemetry.TrackPublishedUpdate(context.Background(), t.PublisherID(),
				&livekit.TrackInfo{
					Sid:       string(t.ID()),
					Type:      livekit.TrackType_VIDEO,
					Muted:     t.IsMuted(),
					Width:     layer.Width,
					Height:    layer.Height,
					Simulcast: t.IsSimulcast(),
				})
		}
	})

	t.connectionStats = connectionquality.NewConnectionStats(connectionquality.ConnectionStatsParams{
		UpdateInterval: connectionQualityUpdateInterval,
		CodecType:      track.Kind(),
		GetTotalBytes: func() uint64 {
			receiver := t.Receiver()
			if receiver != nil {
				return receiver.(*sfu.WebRTCReceiver).GetTotalBytes()
			}

			return 0
		},
		GetIsReducedQuality: func() bool {
			publishing, expected := t.getNumUpTracks()
			return publishing < expected
		},
		Logger: t.params.Logger,
	})
	t.connectionStats.OnStatsUpdate(func(_cs *connectionquality.ConnectionStats, stat *livekit.AnalyticsStat) {
		t.params.Telemetry.TrackStats(livekit.StreamType_UPSTREAM, t.PublisherID(), t.ID(), stat)
	})

	t.AddOnClose(func() {
		t.connectionStats.Close()
	})
	return t
}

func (t *MediaTrack) SignalCid() string {
	return t.params.SignalCid
}

func (t *MediaTrack) SdpCid() string {
	return t.params.SdpCid
}

func (t *MediaTrack) ToProto() *livekit.TrackInfo {
	info := t.MediaTrackReceiver.TrackInfo()
	info.Muted = t.IsMuted()
	info.Simulcast = t.IsSimulcast()
	layers := t.MediaTrackReceiver.GetVideoLayers()
	for _, layer := range layers {
		if int(layer.Quality) < len(t.layerSSRCs) {
			layer.Ssrc = t.layerSSRCs[layer.Quality]
		}
	}
	info.Layers = layers

	return info
}

func (t *MediaTrack) getNumUpTracks() (uint32, uint32) {
	numExpected := atomic.LoadUint32(&t.numUpTracks)

	numSubscribedLayers := t.numSubscribedLayers()
	if numSubscribedLayers < numExpected {
		numExpected = numSubscribedLayers
	}

	numPublishing := uint32(0)
	receiver := t.Receiver()
	if receiver != nil {
		numPublishing = uint32(receiver.(*sfu.WebRTCReceiver).NumAvailableSpatialLayers())
	}

	return numPublishing, numExpected
}

// AddReceiver adds a new RTP receiver to the track
func (t *MediaTrack) AddReceiver(receiver *webrtc.RTPReceiver, track *webrtc.TrackRemote, twcc *twcc.Responder) {
	buff, rtcpReader := t.params.BufferFactory.GetBufferPair(uint32(track.SSRC()))
	if buff == nil || rtcpReader == nil {
		t.params.Logger.Errorw("could not retrieve buffer pair", nil)
		return
	}
	buff.OnFeedback(t.handlePublisherFeedback)

	if t.Kind() == livekit.TrackType_AUDIO {
		t.audioLevelMu.Lock()
		t.audioLevel = NewAudioLevel(t.params.AudioConfig.ActiveLevel, t.params.AudioConfig.MinPercentile)
		buff.OnAudioLevel(func(level uint8, duration uint32) {
			t.audioLevelMu.RLock()
			defer t.audioLevelMu.RUnlock()

			t.audioLevel.Observe(level, duration)
		})
		t.audioLevelMu.Unlock()
	} else if t.Kind() == livekit.TrackType_VIDEO {
		if twcc != nil {
			buff.OnTransportWideCC(func(sn uint16, timeNS int64, marker bool) {
				twcc.Push(sn, timeNS, marker)
			})
		}
	}

	rtcpReader.OnPacket(func(bytes []byte) {
		pkts, err := rtcp.Unmarshal(bytes)
		if err != nil {
			t.params.Logger.Errorw("could not unmarshal RTCP", err)
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
	if t.Receiver() == nil {
		wr := sfu.NewWebRTCReceiver(
			receiver,
			track,
			t.PublisherID(),
			sfu.WithPliThrottle(0),
			sfu.WithLoadBalanceThreshold(20),
			sfu.WithStreamTrackers(),
		)
		wr.SetRTCPCh(t.params.RTCPChan)
		wr.OnCloseHandler(func() {
			t.RemoveAllSubscribers()
			t.MediaTrackReceiver.Close()
			t.params.Telemetry.TrackUnpublished(context.Background(), t.PublisherID(), t.ToProto(), uint32(track.SSRC()))
		})
		t.params.Telemetry.TrackPublished(context.Background(), t.PublisherID(), t.ToProto())

		t.buffer = buff

		t.MediaTrackReceiver.SetupReceiver(wr)

		t.connectionStats.Start()
	}
	t.lock.Unlock()

	t.Receiver().(*sfu.WebRTCReceiver).AddUpTrack(track, buff)

	atomic.AddUint32(&t.numUpTracks, 1)
	// LK-TODO: can remove this completely when VideoLayers protocol becomes the default as it has info from client or if we decide to use TrackInfo.Simulcast
	if atomic.LoadUint32(&t.numUpTracks) > 1 || track.RID() != "" {
		// cannot only rely on numUpTracks since we fire metadata events immediately after the first layer
		t.MediaTrackReceiver.SetSimulcast(true)
	}

	if t.IsSimulcast() {
		layer := sfu.RidToLayer(track.RID())
		if int(layer) < len(t.layerSSRCs) {
			t.layerSSRCs[layer] = uint32(track.SSRC())
		}
	}

	buff.Bind(receiver.GetParameters(), track.Codec().RTPCodecCapability, buffer.Options{
		MaxBitRate: t.params.ReceiverConfig.maxBitrate,
	})
}

func (t *MediaTrack) TrySetSimulcastSSRC(layer uint8, ssrc uint32) {
	if int(layer) < len(t.layerSSRCs) && t.layerSSRCs[layer] == 0 {
		t.layerSSRCs[layer] = ssrc
	}
}

func (t *MediaTrack) GetAudioLevel() (level uint8, active bool) {
	t.audioLevelMu.RLock()
	defer t.audioLevelMu.RUnlock()

	if t.audioLevel == nil {
		return SilentAudioLevel, false
	}
	return t.audioLevel.GetLevel()
}

func (t *MediaTrack) handlePublisherFeedback(packets []rtcp.Packet) {
	t.connectionStats.RTCPFeedback(packets, 0)

	// also look for sender reports
	// feedback for the source RTCP
	t.params.RTCPChan <- packets
}

func (t *MediaTrack) GetConnectionScore() float64 {
	return t.connectionStats.GetScore()
}
