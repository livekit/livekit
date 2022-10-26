package rtc

import (
	"context"
	"strings"
	"sync"

	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"
	"go.uber.org/atomic"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/mediatransportutil/pkg/twcc"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/sfu"
	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/telemetry"
)

// MediaTrack represents a WebRTC track that needs to be forwarded
// Implements MediaTrack and PublishedTrack interface
type MediaTrack struct {
	params      MediaTrackParams
	numUpTracks atomic.Uint32
	buffer      *buffer.Buffer

	*MediaTrackReceiver
	*MediaLossProxy

	dynacastManager *DynacastManager

	lock sync.RWMutex
}

type MediaTrackParams struct {
	TrackInfo           *livekit.TrackInfo
	SignalCid           string
	SdpCid              string
	ParticipantID       livekit.ParticipantID
	ParticipantIdentity livekit.ParticipantIdentity
	ParticipantVersion  uint32
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
	SimTracks         map[uint32]SimulcastTrackInfo
}

func NewMediaTrack(params MediaTrackParams) *MediaTrack {
	t := &MediaTrack{
		params: params,
	}

	t.MediaTrackReceiver = NewMediaTrackReceiver(MediaTrackReceiverParams{
		TrackInfo:           params.TrackInfo,
		MediaTrack:          t,
		IsRelayed:           false,
		ParticipantID:       params.ParticipantID,
		ParticipantIdentity: params.ParticipantIdentity,
		ParticipantVersion:  params.ParticipantVersion,
		BufferFactory:       params.BufferFactory,
		ReceiverConfig:      params.ReceiverConfig,
		SubscriberConfig:    params.SubscriberConfig,
		Telemetry:           params.Telemetry,
		Logger:              params.Logger,
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

	if params.TrackInfo.Type == livekit.TrackType_AUDIO {
		t.MediaLossProxy = NewMediaLossProxy(MediaLossProxyParams{
			Logger: params.Logger,
		})
		t.MediaLossProxy.OnMediaLossUpdate(func(fractionalLoss uint8) {
			if t.buffer != nil {
				// ok to access buffer since receivers are added before subscribers
				t.buffer.SetLastFractionLostReport(fractionalLoss)
			}
		})
		t.MediaTrackReceiver.OnMediaLossFeedback(t.MediaLossProxy.HandleMaxLossFeedback)
	}

	if params.TrackInfo.Type == livekit.TrackType_VIDEO {
		t.dynacastManager = NewDynacastManager(DynacastManagerParams{
			DynacastPauseDelay: params.VideoConfig.DynacastPauseDelay,
			Logger:             params.Logger,
		})
		t.MediaTrackReceiver.OnSetupReceiver(func(mime string) {
			t.dynacastManager.AddCodec(mime)
		})
		t.MediaTrackReceiver.OnSubscriberMaxQualityChange(
			func(subscriberID livekit.ParticipantID, codec webrtc.RTPCodecCapability, layer int32) {
				t.dynacastManager.NotifySubscriberMaxQuality(
					subscriberID,
					codec.MimeType,
					buffer.SpatialLayerToVideoQuality(layer, t.params.TrackInfo),
				)
			},
		)
	}

	return t
}

func (t *MediaTrack) OnSubscribedMaxQualityChange(
	f func(
		trackID livekit.TrackID,
		subscribedQualities []*livekit.SubscribedCodec,
		maxSubscribedQualities []types.SubscribedCodecQuality,
	) error,
) {
	if t.dynacastManager == nil {
		return
	}

	handler := func(subscribedQualities []*livekit.SubscribedCodec, maxSubscribedQualities []types.SubscribedCodecQuality) {
		if f != nil && !t.IsMuted() {
			_ = f(t.ID(), subscribedQualities, maxSubscribedQualities)
		}

		for _, q := range maxSubscribedQualities {
			receiver := t.Receiver(q.CodecMime)
			if receiver != nil {
				receiver.SetMaxExpectedSpatialLayer(buffer.VideoQualityToSpatialLayer(q.Quality, t.params.TrackInfo))
			}
		}
	}

	t.dynacastManager.OnSubscribedMaxQualityChange(handler)
}

func (t *MediaTrack) NotifySubscriberNodeMaxQuality(nodeID livekit.NodeID, qualities []types.SubscribedCodecQuality) {
	if t.dynacastManager != nil {
		t.dynacastManager.NotifySubscriberNodeMaxQuality(nodeID, qualities)
	}
}

func (t *MediaTrack) SignalCid() string {
	return t.params.SignalCid
}

func (t *MediaTrack) HasSdpCid(cid string) bool {
	if t.params.SdpCid == cid {
		return true
	}

	info := t.params.TrackInfo
	for _, c := range info.Codecs {
		if c.Cid == cid {
			return true
		}
	}
	return false
}

func (t *MediaTrack) ToProto() *livekit.TrackInfo {
	info := t.MediaTrackReceiver.TrackInfo(true)
	info.Muted = t.IsMuted()
	info.Simulcast = t.IsSimulcast()
	return info
}

func (t *MediaTrack) SetPendingCodecSid(codecs []*livekit.SimulcastCodec) {
	ti := proto.Clone(t.params.TrackInfo).(*livekit.TrackInfo)
	for _, c := range codecs {
		for _, origin := range ti.Codecs {
			if strings.Contains(origin.MimeType, c.Codec) {
				origin.Cid = c.Cid
				break
			}
		}
	}
	t.params.TrackInfo = ti
}

// AddReceiver adds a new RTP receiver to the track, returns true when receiver represents a new codec
func (t *MediaTrack) AddReceiver(receiver *webrtc.RTPReceiver, track *webrtc.TrackRemote, twcc *twcc.Responder, mid string) bool {
	var newCodec bool
	buff, rtcpReader := t.params.BufferFactory.GetBufferPair(uint32(track.SSRC()))
	if buff == nil || rtcpReader == nil {
		t.params.Logger.Errorw("could not retrieve buffer pair", nil)
		return newCodec
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
			t.params.TrackInfo,
			LoggerWithCodecMime(t.params.Logger, mime),
			twcc,
			sfu.WithPliThrottleConfig(t.params.PLIThrottleConfig),
			sfu.WithAudioConfig(t.params.AudioConfig),
			sfu.WithLoadBalanceThreshold(20),
			sfu.WithStreamTrackers(),
		)
		newWR.SetRTCPCh(t.params.RTCPChan)
		newWR.OnCloseHandler(func() {
			t.MediaTrackReceiver.ClearReceiver(mime, false)
			if t.MediaTrackReceiver.TryClose() {
				if t.dynacastManager != nil {
					t.dynacastManager.Close()
				}
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
			// LK-TODO: this needs to be receiver/mime aware
			t.params.Telemetry.TrackStats(livekit.StreamType_UPSTREAM, t.PublisherID(), t.ID(), stat)
		})
		if t.PrimaryReceiver() == nil {
			// primary codec published, set potential codecs
			potentialCodecs := make([]webrtc.RTPCodecParameters, 0, len(t.params.TrackInfo.Codecs))
			parameters := receiver.GetParameters()
			for _, c := range t.params.TrackInfo.Codecs {
				for _, nc := range parameters.Codecs {
					if strings.EqualFold(nc.MimeType, c.MimeType) {
						potentialCodecs = append(potentialCodecs, nc)
						break
					}
				}
			}

			if len(potentialCodecs) > 0 {
				t.params.Logger.Debugw("primary codec published, set potential codecs", "potential", potentialCodecs)
				t.MediaTrackReceiver.SetPotentialCodecs(potentialCodecs, parameters.HeaderExtensions)
			}
			t.params.Telemetry.TrackPublished(
				context.Background(),
				t.PublisherID(),
				t.PublisherIdentity(),
				t.ToProto(),
			)
		}

		newWR.OnMaxLayerChange(t.onMaxLayerChange)

		t.buffer = buff

		t.MediaTrackReceiver.SetupReceiver(newWR, priority, mid)

		for ssrc, info := range t.params.SimTracks {
			if info.Mid == mid {
				t.MediaTrackReceiver.SetLayerSsrc(mime, info.Rid, ssrc)
			}
		}
		wr = newWR
		newCodec = true
	}
	t.lock.Unlock()

	wr.(*sfu.WebRTCReceiver).AddUpTrack(track, buff)

	// LK-TODO: can remove this completely when VideoLayers protocol becomes the default as it has info from client or if we decide to use TrackInfo.Simulcast
	if t.numUpTracks.Inc() > 1 || track.RID() != "" {
		// cannot only rely on numUpTracks since we fire metadata events immediately after the first layer
		t.SetSimulcast(true)
	}

	if t.IsSimulcast() {
		t.MediaTrackReceiver.SetLayerSsrc(mime, track.RID(), uint32(track.SSRC()))
	}

	buff.Bind(receiver.GetParameters(), track.Codec().RTPCodecCapability)

	// if subscriber request fps before fps calculated, update them after fps updated.
	buff.OnFpsChanged(func() {
		t.MediaTrackSubscriptions.UpdateVideoLayers()
	})
	return newCodec
}

func (t *MediaTrack) GetConnectionScore() float32 {
	receiver := t.PrimaryReceiver()
	if rtcReceiver, ok := receiver.(*sfu.WebRTCReceiver); ok {
		return rtcReceiver.GetConnectionScore()
	}
	return 0.0
}

func (t *MediaTrack) SetRTT(rtt uint32) {
	t.MediaTrackReceiver.SetRTT(rtt)
}

func (t *MediaTrack) HasPendingCodec() bool {
	return t.MediaTrackReceiver.PrimaryReceiver() == nil
}

func (t *MediaTrack) onMaxLayerChange(maxLayer int32) {
	ti := &livekit.TrackInfo{
		Sid:  t.trackInfo.Sid,
		Type: t.trackInfo.Type,
	}

	if layer, ok := t.MediaTrackReceiver.layerDimensions[livekit.VideoQuality(maxLayer)]; ok {
		ti.Layers = []*livekit.VideoLayer{{Quality: livekit.VideoQuality(maxLayer), Width: layer.Width, Height: layer.Height}}
	} else if maxLayer == -1 {
		ti.Layers = []*livekit.VideoLayer{{Quality: livekit.VideoQuality_OFF}}
	}
	t.params.Telemetry.TrackPublishedUpdate(context.Background(), t.PublisherID(), ti)
}

func (t *MediaTrack) Restart() {
	t.MediaTrackReceiver.Restart()

	if t.dynacastManager != nil {
		t.dynacastManager.Restart()
	}
}

func (t *MediaTrack) Close(willBeResumed bool) {
	if t.dynacastManager != nil {
		t.dynacastManager.Close()
	}

	t.MediaTrackReceiver.ClearAllReceivers(willBeResumed)
	t.MediaTrackReceiver.Close()
}

func (t *MediaTrack) SetMuted(muted bool) {
	// update quality based on subscription if unmuting.
	// This will queue up the current state, but subscriber
	// driven changes could update it.
	if !muted && t.dynacastManager != nil {
		t.dynacastManager.ForceUpdate()
	}

	t.MediaTrackReceiver.SetMuted(muted)
}
