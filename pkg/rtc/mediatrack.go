package rtc

import (
	"context"
	"strings"
	"sync"

	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"
	"go.uber.org/atomic"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/sfu"
	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/sfu/twcc"
	"github.com/livekit/livekit-server/pkg/telemetry"
)

// MediaTrack represents a WebRTC track that needs to be forwarded
// Implements MediaTrack and PublishedTrack interface
type MediaTrack struct {
	params      MediaTrackParams
	numUpTracks atomic.Uint32
	buffer      *buffer.Buffer

	layerSSRCs [livekit.VideoQuality_HIGH + 1]uint32

	audioLevelMu sync.RWMutex
	audioLevel   *AudioLevel

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
	RTCPChan          chan []rtcp.Packet
	BufferFactory     *buffer.Factory
	ReceiverConfig    ReceiverConfig
	SubscriberConfig  DirectionConfig
	PLIThrottleConfig config.PLIThrottleConfig
	AudioConfig       config.AudioConfig
	VideoConfig       config.VideoConfig
	Telemetry         telemetry.TelemetryService
	Logger            logger.Logger
}

func NewMediaTrack(params MediaTrackParams) *MediaTrack {
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
		VideoConfig:         params.VideoConfig,
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
		t.params.Telemetry.TrackPublishedUpdate(context.Background(), t.PublisherID(),
			&livekit.TrackInfo{
				Sid:       string(t.ID()),
				Type:      livekit.TrackType_VIDEO,
				Muted:     t.IsMuted(),
				Simulcast: t.IsSimulcast(),
				Layers:    layers,
			})
	})

	return t
}

func (t *MediaTrack) SignalCid() string {
	return t.params.SignalCid
}

func (t *MediaTrack) HasSdpCid(cid string) bool {
	if t.params.SdpCid == cid {
		return true
	}

	info := t.MediaTrackReceiver.TrackInfo()
	t.params.Logger.Debugw("MediaTrack.HasSdpCid", "cid", cid, "trackInfo", info.String())
	for _, c := range info.Codecs {
		if c.Cid == cid {
			return true
		}
	}
	return false
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

// AddReceiver adds a new RTP receiver to the track
func (t *MediaTrack) AddReceiver(receiver *webrtc.RTPReceiver, track *webrtc.TrackRemote, twcc *twcc.Responder) {
	buff, rtcpReader := t.params.BufferFactory.GetBufferPair(uint32(track.SSRC()))
	if buff == nil || rtcpReader == nil {
		t.params.Logger.Errorw("could not retrieve buffer pair", nil)
		return
	}

	if t.Kind() == livekit.TrackType_AUDIO {
		t.audioLevelMu.Lock()
		t.audioLevel = NewAudioLevel(t.params.AudioConfig.ActiveLevel, t.params.AudioConfig.MinPercentile, t.params.AudioConfig.UpdateInterval)
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
	mime := strings.ToLower(track.Codec().MimeType)
	t.params.Logger.Debugw("AddReceiver", "mime", track.Codec().MimeType)
	wr := t.MediaTrackReceiver.Receiver(mime)
	if wr == nil {
		var priority int
		for idx, c := range t.params.TrackInfo.Codecs {
			if strings.HasSuffix(mime, c.MimeType) {
				priority = idx
				break
			}
		}
		newWR := sfu.NewWebRTCReceiver(
			receiver,
			track,
			t.PublisherID(),
			t.params.TrackInfo.Source,
			LoggerWithCodecMime(t.params.Logger, mime),
			sfu.WithPliThrottle(t.params.PLIThrottleConfig),
			sfu.WithLoadBalanceThreshold(20),
			sfu.WithStreamTrackers(),
		)
		newWR.SetRTCPCh(t.params.RTCPChan)
		newWR.OnCloseHandler(func() {
			t.RemoveAllSubscribers()
			t.MediaTrackReceiver.ClearReceiver(mime)
			if t.MediaTrackReceiver.TryClose() {
				t.params.Telemetry.TrackUnpublished(
					context.Background(),
					t.PublisherID(),
					t.PublisherIdentity(),
					t.ToProto(),
					uint32(track.SSRC()),
				)
			}
		})
		newWR.OnStatsUpdate(func(_ *sfu.WebRTCReceiver, stat *livekit.AnalyticsStat) {
			t.params.Telemetry.TrackStats(livekit.StreamType_UPSTREAM, t.PublisherID(), t.ID(), stat)
		})
		if t.PrimaryReceiver() == nil {
			t.params.Telemetry.TrackPublished(
				context.Background(),
				t.PublisherID(),
				t.PublisherIdentity(),
				t.ToProto(),
			)
		}

		t.buffer = buff

		t.MediaTrackReceiver.SetupReceiver(newWR, priority)
		wr = newWR
	}
	t.lock.Unlock()

	wr.(*sfu.WebRTCReceiver).AddUpTrack(track, buff)

	// LK-TODO: can remove this completely when VideoLayers protocol becomes the default as it has info from client or if we decide to use TrackInfo.Simulcast
	if t.numUpTracks.Inc() > 1 || track.RID() != "" {
		// cannot only rely on numUpTracks since we fire metadata events immediately after the first layer
		t.SetSimulcast(true)
	}

	if t.IsSimulcast() {
		// TODO : seprate svc layers from one track
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

func (t *MediaTrack) GetConnectionScore() float32 {
	receiver := t.PrimaryReceiver()
	if receiver == nil {
		return 0.0
	}

	return receiver.(*sfu.WebRTCReceiver).GetConnectionScore()
}

func (t *MediaTrack) SetRTT(rtt uint32) {
	t.MediaTrackReceiver.SetRTT(rtt)
}
