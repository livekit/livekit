// Copyright 2023 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package rtc

import (
	"context"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
	"go.uber.org/atomic"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/rtc/dynacast"
	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/sfu"
	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/sfu/connectionquality"
	"github.com/livekit/livekit-server/pkg/telemetry"
	util "github.com/livekit/mediatransportutil"
)

// MediaTrack represents a WebRTC track that needs to be forwarded
// Implements MediaTrack and PublishedTrack interface
type MediaTrack struct {
	params         MediaTrackParams
	numUpTracks    atomic.Uint32
	buffer         *buffer.Buffer
	everSubscribed atomic.Bool

	*MediaTrackReceiver
	*MediaLossProxy

	dynacastManager *dynacast.DynacastManager

	lock sync.RWMutex

	rttFromXR atomic.Bool
}

type MediaTrackParams struct {
	SignalCid             string
	SdpCid                string
	ParticipantID         livekit.ParticipantID
	ParticipantIdentity   livekit.ParticipantIdentity
	ParticipantVersion    uint32
	BufferFactory         *buffer.Factory
	ReceiverConfig        ReceiverConfig
	SubscriberConfig      DirectionConfig
	PLIThrottleConfig     sfu.PLIThrottleConfig
	AudioConfig           sfu.AudioConfig
	VideoConfig           config.VideoConfig
	Telemetry             telemetry.TelemetryService
	Logger                logger.Logger
	SimTracks             map[uint32]SimulcastTrackInfo
	OnRTCP                func([]rtcp.Packet)
	ForwardStats          *sfu.ForwardStats
	OnTrackEverSubscribed func(livekit.TrackID)
}

func NewMediaTrack(params MediaTrackParams, ti *livekit.TrackInfo) *MediaTrack {
	t := &MediaTrack{
		params: params,
	}

	t.MediaTrackReceiver = NewMediaTrackReceiver(MediaTrackReceiverParams{
		MediaTrack:          t,
		IsRelayed:           false,
		ParticipantID:       params.ParticipantID,
		ParticipantIdentity: params.ParticipantIdentity,
		ParticipantVersion:  params.ParticipantVersion,
		ReceiverConfig:      params.ReceiverConfig,
		SubscriberConfig:    params.SubscriberConfig,
		AudioConfig:         params.AudioConfig,
		Telemetry:           params.Telemetry,
		Logger:              params.Logger,
	}, ti)

	if ti.Type == livekit.TrackType_AUDIO {
		t.MediaLossProxy = NewMediaLossProxy(MediaLossProxyParams{
			Logger: params.Logger,
		})
		t.MediaLossProxy.OnMediaLossUpdate(func(fractionalLoss uint8) {
			if t.buffer != nil {
				t.buffer.SetLastFractionLostReport(fractionalLoss)
			}
		})
		t.MediaTrackReceiver.OnMediaLossFeedback(t.MediaLossProxy.HandleMaxLossFeedback)
	}

	if ti.Type == livekit.TrackType_VIDEO {
		t.dynacastManager = dynacast.NewDynacastManager(dynacast.DynacastManagerParams{
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
					buffer.SpatialLayerToVideoQuality(layer, t.MediaTrackReceiver.TrackInfo()),
				)
			},
		)
	}

	return t
}

func (t *MediaTrack) OnSubscribedMaxQualityChange(
	f func(
		trackID livekit.TrackID,
		trackInfo *livekit.TrackInfo,
		subscribedQualities []*livekit.SubscribedCodec,
		maxSubscribedQualities []types.SubscribedCodecQuality,
	) error,
) {
	if t.dynacastManager == nil {
		return
	}

	handler := func(subscribedQualities []*livekit.SubscribedCodec, maxSubscribedQualities []types.SubscribedCodecQuality) {
		if f != nil && !t.IsMuted() {
			_ = f(t.ID(), t.ToProto(), subscribedQualities, maxSubscribedQualities)
		}

		for _, q := range maxSubscribedQualities {
			receiver := t.Receiver(q.CodecMime)
			if receiver != nil {
				receiver.SetMaxExpectedSpatialLayer(buffer.VideoQualityToSpatialLayer(q.Quality, t.MediaTrackReceiver.TrackInfo()))
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

	ti := t.MediaTrackReceiver.TrackInfoClone()
	for _, c := range ti.Codecs {
		if c.Cid == cid {
			return true
		}
	}
	return false
}

func (t *MediaTrack) ToProto() *livekit.TrackInfo {
	return t.MediaTrackReceiver.TrackInfoClone()
}

func (t *MediaTrack) UpdateCodecCid(codecs []*livekit.SimulcastCodec) {
	t.MediaTrackReceiver.UpdateCodecCid(codecs)
}

// AddReceiver adds a new RTP receiver to the track, returns true when receiver represents a new codec
func (t *MediaTrack) AddReceiver(receiver *webrtc.RTPReceiver, track sfu.TrackRemote, mid string) bool {
	var newCodec bool
	ssrc := uint32(track.SSRC())
	buff, rtcpReader := t.params.BufferFactory.GetBufferPair(ssrc)
	if buff == nil || rtcpReader == nil {
		t.params.Logger.Errorw("could not retrieve buffer pair", nil)
		return newCodec
	}

	var lastRR uint32
	rtcpReader.OnPacket(func(bytes []byte) {
		pkts, err := rtcp.Unmarshal(bytes)
		if err != nil {
			t.params.Logger.Errorw("could not unmarshal RTCP", err)
			return
		}

		for _, pkt := range pkts {
			switch pkt := pkt.(type) {
			case *rtcp.SourceDescription:
			case *rtcp.SenderReport:
				if pkt.SSRC == uint32(track.SSRC()) {
					buff.SetSenderReportData(pkt.RTPTime, pkt.NTPTime, pkt.PacketCount, pkt.OctetCount)
				}
			case *rtcp.ExtendedReport:
			rttFromXR:
				for _, report := range pkt.Reports {
					if rr, ok := report.(*rtcp.DLRRReportBlock); ok {
						for _, dlrrReport := range rr.Reports {
							if dlrrReport.LastRR <= lastRR {
								continue
							}
							nowNTP := util.ToNtpTime(time.Now())
							nowNTP32 := uint32(nowNTP >> 16)
							ntpDiff := nowNTP32 - dlrrReport.LastRR - dlrrReport.DLRR
							rtt := uint32(math.Ceil(float64(ntpDiff) * 1000.0 / 65536.0))
							buff.SetRTT(rtt)
							t.rttFromXR.Store(true)
							lastRR = dlrrReport.LastRR
							break rttFromXR
						}
					}
				}
			}
		}
	})

	ti := t.MediaTrackReceiver.TrackInfoClone()
	t.lock.Lock()
	mime := strings.ToLower(track.Codec().MimeType)
	layer := buffer.RidToSpatialLayer(track.RID(), ti)
	t.params.Logger.Debugw(
		"AddReceiver",
		"rid", track.RID(),
		"layer", layer,
		"ssrc", track.SSRC(),
		"codec", track.Codec(),
	)
	wr := t.MediaTrackReceiver.Receiver(mime)
	if wr == nil {
		priority := -1
		for idx, c := range ti.Codecs {
			if strings.EqualFold(mime, c.MimeType) {
				priority = idx
				break
			}
		}
		if priority < 0 {
			switch len(ti.Codecs) {
			case 0:
				// audio track
				priority = 0
			case 1:
				// older clients or non simulcast-codec, mime type only set later
				if ti.Codecs[0].MimeType == "" {
					priority = 0
				}
			}
		}
		if priority < 0 {
			t.params.Logger.Warnw("could not find codec for webrtc receiver", nil, "webrtcCodec", mime, "track", logger.Proto(ti))
			t.lock.Unlock()
			return false
		}

		newWR := sfu.NewWebRTCReceiver(
			receiver,
			track,
			ti,
			LoggerWithCodecMime(t.params.Logger, mime),
			t.params.OnRTCP,
			t.params.VideoConfig.StreamTrackerManager,
			sfu.WithPliThrottleConfig(t.params.PLIThrottleConfig),
			sfu.WithAudioConfig(t.params.AudioConfig),
			sfu.WithLoadBalanceThreshold(20),
			sfu.WithStreamTrackers(),
			sfu.WithForwardStats(t.params.ForwardStats),
		)
		newWR.OnCloseHandler(func() {
			t.MediaTrackReceiver.SetClosing()
			t.MediaTrackReceiver.ClearReceiver(mime, false)
			if t.MediaTrackReceiver.TryClose() {
				if t.dynacastManager != nil {
					t.dynacastManager.Close()
				}
			}
		})
		// SIMULCAST-CODEC-TODO: these need to be receiver/mime aware, setting it up only for primary now
		if priority == 0 {
			newWR.OnStatsUpdate(func(_ *sfu.WebRTCReceiver, stat *livekit.AnalyticsStat) {
				key := telemetry.StatsKeyForTrack(livekit.StreamType_UPSTREAM, t.PublisherID(), t.ID(), ti.Source, ti.Type)
				t.params.Telemetry.TrackStats(key, stat)
			})

			newWR.OnMaxLayerChange(t.onMaxLayerChange)
		}
		if t.PrimaryReceiver() == nil {
			// primary codec published, set potential codecs
			potentialCodecs := make([]webrtc.RTPCodecParameters, 0, len(ti.Codecs))
			parameters := receiver.GetParameters()
			for _, c := range ti.Codecs {
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
		}

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

	if err := wr.(*sfu.WebRTCReceiver).AddUpTrack(track, buff); err != nil {
		t.params.Logger.Warnw(
			"adding up track failed", err,
			"rid", track.RID(),
			"layer", layer,
			"ssrc", track.SSRC(),
			"newCodec", newCodec,
		)
		buff.Close()
		return false
	}

	// LK-TODO: can remove this completely when VideoLayers protocol becomes the default as it has info from client or if we decide to use TrackInfo.Simulcast
	if t.numUpTracks.Inc() > 1 || track.RID() != "" {
		// cannot only rely on numUpTracks since we fire metadata events immediately after the first layer
		t.SetSimulcast(true)
	}

	var bitrates int
	if len(ti.Layers) > int(layer) {
		bitrates = int(ti.Layers[layer].GetBitrate())
	}

	t.MediaTrackReceiver.SetLayerSsrc(mime, track.RID(), uint32(track.SSRC()))

	buff.Bind(receiver.GetParameters(), track.Codec().RTPCodecCapability, bitrates)

	// if subscriber request fps before fps calculated, update them after fps updated.
	buff.OnFpsChanged(func() {
		t.MediaTrackSubscriptions.UpdateVideoLayers()
	})

	buff.OnFinalRtpStats(func(stats *livekit.RTPStats) {
		t.params.Telemetry.TrackPublishRTPStats(
			context.Background(),
			t.params.ParticipantID,
			t.ID(),
			mime,
			int(layer),
			stats,
		)
	})
	return newCodec
}

func (t *MediaTrack) GetConnectionScoreAndQuality() (float32, livekit.ConnectionQuality) {
	receiver := t.PrimaryReceiver()
	if rtcReceiver, ok := receiver.(*sfu.WebRTCReceiver); ok {
		return rtcReceiver.GetConnectionScoreAndQuality()
	}

	return connectionquality.MaxMOS, livekit.ConnectionQuality_EXCELLENT
}

func (t *MediaTrack) SetRTT(rtt uint32) {
	if !t.rttFromXR.Load() {
		t.MediaTrackReceiver.SetRTT(rtt)
	}
}

func (t *MediaTrack) HasPendingCodec() bool {
	return t.MediaTrackReceiver.PrimaryReceiver() == nil
}

func (t *MediaTrack) onMaxLayerChange(maxLayer int32) {
	t.MediaTrackReceiver.NotifyMaxLayerChange(maxLayer)
}

func (t *MediaTrack) Restart() {
	t.MediaTrackReceiver.Restart()

	if t.dynacastManager != nil {
		t.dynacastManager.Restart()
	}
}

func (t *MediaTrack) Close(isExpectedToResume bool) {
	t.MediaTrackReceiver.SetClosing()
	if t.dynacastManager != nil {
		t.dynacastManager.Close()
	}
	t.MediaTrackReceiver.ClearAllReceivers(isExpectedToResume)
	t.MediaTrackReceiver.Close(isExpectedToResume)
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

func (t *MediaTrack) OnTrackSubscribed() {
	if !t.everSubscribed.Swap(true) && t.params.OnTrackEverSubscribed != nil {
		go t.params.OnTrackEverSubscribed(t.ID())
	}
}
