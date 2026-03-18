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
	"math"
	"sync"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
	"go.uber.org/atomic"

	"github.com/livekit/protocol/codecs/mime"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/observability/roomobs"
	"github.com/livekit/protocol/utils/mono"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/rtc/dynacast"
	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/sfu"
	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/sfu/connectionquality"
	"github.com/livekit/livekit-server/pkg/sfu/interceptor"
	"github.com/livekit/livekit-server/pkg/telemetry"
	util "github.com/livekit/mediatransportutil"
)

var _ types.LocalMediaTrack = (*MediaTrack)(nil)

// MediaTrack represents a WebRTC track that needs to be forwarded
// Implements MediaTrack and PublishedTrack interface
type MediaTrack struct {
	params         MediaTrackParams
	buffer         *buffer.Buffer
	everSubscribed atomic.Bool

	*MediaTrackReceiver
	*MediaLossProxy

	dynacastManager dynacast.DynacastManager

	lock sync.RWMutex

	rttFromXR atomic.Bool

	backupCodecPolicy             livekit.BackupCodecPolicy
	regressionTargetCodec         mime.MimeType
	regressionTargetCodecReceived bool

	onSubscribedMaxQualityChange func(
		trackID livekit.TrackID,
		trackInfo *livekit.TrackInfo,
		subscribedQualities []*livekit.SubscribedCodec,
		maxSubscribedQualities []types.SubscribedCodecQuality,
	) error
	onSubscribedAudioCodecChange func(
		trackID livekit.TrackID,
		codecs []*livekit.SubscribedAudioCodec,
	) error
}

type MediaTrackParams struct {
	ParticipantID                    func() livekit.ParticipantID
	ParticipantIdentity              livekit.ParticipantIdentity
	ParticipantVersion               uint32
	ParticipantCountry               string
	BufferFactory                    *buffer.Factory
	ReceiverConfig                   ReceiverConfig
	SubscriberConfig                 DirectionConfig
	PLIThrottleConfig                sfu.PLIThrottleConfig
	AudioConfig                      sfu.AudioConfig
	VideoConfig                      config.VideoConfig
	TelemetryListener                types.ParticipantTelemetryListener
	Logger                           logger.Logger
	Reporter                         roomobs.TrackReporter
	SimTracks                        map[uint32]interceptor.SimulcastTrackInfo
	OnRTCP                           func([]rtcp.Packet)
	ForwardStats                     *sfu.ForwardStats
	OnTrackEverSubscribed            func(livekit.TrackID)
	ShouldRegressCodec               func() bool
	PreferVideoSizeFromMedia         bool
	EnableRTPStreamRestartDetection  bool
	UpdateTrackInfoByVideoSizeChange bool
	ForceBackupCodecPolicySimulcast  bool
}

func NewMediaTrack(params MediaTrackParams, ti *livekit.TrackInfo) *MediaTrack {
	t := &MediaTrack{
		params:            params,
		backupCodecPolicy: ti.BackupCodecPolicy,
	}

	if t.params.ForceBackupCodecPolicySimulcast {
		t.backupCodecPolicy = livekit.BackupCodecPolicy_SIMULCAST
	}

	if t.backupCodecPolicy != livekit.BackupCodecPolicy_SIMULCAST && len(ti.Codecs) > 1 {
		t.regressionTargetCodec = mime.NormalizeMimeType(ti.Codecs[1].MimeType)
		t.params.Logger.Debugw("track enabled codec regression", "regressionCodec", t.regressionTargetCodec)
	}

	t.MediaTrackReceiver = NewMediaTrackReceiver(MediaTrackReceiverParams{
		MediaTrack:               t,
		IsRelayed:                false,
		ParticipantID:            params.ParticipantID,
		ParticipantIdentity:      params.ParticipantIdentity,
		ParticipantVersion:       params.ParticipantVersion,
		ReceiverConfig:           params.ReceiverConfig,
		SubscriberConfig:         params.SubscriberConfig,
		AudioConfig:              params.AudioConfig,
		TelemetryListener:        params.TelemetryListener,
		Logger:                   params.Logger,
		RegressionTargetCodec:    t.regressionTargetCodec,
		PreferVideoSizeFromMedia: params.PreferVideoSizeFromMedia,
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

	switch ti.Type {
	case livekit.TrackType_VIDEO:
		t.dynacastManager = dynacast.NewDynacastManagerVideo(dynacast.DynacastManagerVideoParams{
			DynacastPauseDelay: params.VideoConfig.DynacastPauseDelay,
			Listener:           t,
			Logger:             params.Logger,
		})

	case livekit.TrackType_AUDIO:
		if len(ti.Codecs) > 1 {
			t.dynacastManager = dynacast.NewDynacastManagerAudio(dynacast.DynacastManagerAudioParams{
				Listener: t,
				Logger:   params.Logger,
			})
		}
	}
	t.MediaTrackReceiver.OnSetupReceiver(func(mime mime.MimeType) {
		if t.dynacastManager != nil {
			t.dynacastManager.AddCodec(mime)
		}
	})
	t.MediaTrackReceiver.OnSubscriberMaxQualityChange(
		func(subscriberID livekit.ParticipantID, mimeType mime.MimeType, layer int32) {
			if t.dynacastManager != nil {
				t.dynacastManager.NotifySubscriberMaxQuality(
					subscriberID,
					mimeType,
					buffer.GetVideoQualityForSpatialLayer(
						mimeType,
						layer,
						t.MediaTrackReceiver.TrackInfo(),
					),
				)
			}
		},
	)
	t.MediaTrackReceiver.OnSubscriberAudioCodecChange(
		func(subscriberID livekit.ParticipantID, mimeType mime.MimeType, enabled bool) {
			if t.dynacastManager != nil {
				t.dynacastManager.NotifySubscription(subscriberID, mimeType, enabled)
			}
		},
	)
	t.MediaTrackReceiver.OnCodecRegression(func(old, new webrtc.RTPCodecParameters) {
		if t.dynacastManager != nil {
			t.dynacastManager.HandleCodecRegression(
				mime.NormalizeMimeType(old.MimeType),
				mime.NormalizeMimeType(new.MimeType),
			)
		}
	})

	t.SetMuted(ti.Muted)
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
	t.lock.Lock()
	t.onSubscribedMaxQualityChange = f
	t.lock.Unlock()
}

func (t *MediaTrack) OnSubscribedAudioCodecChange(
	f func(
		trackID livekit.TrackID,
		codecs []*livekit.SubscribedAudioCodec,
	) error,
) {
	t.lock.Lock()
	t.onSubscribedAudioCodecChange = f
	t.lock.Unlock()
}

func (t *MediaTrack) NotifySubscriberNodeMaxQuality(nodeID livekit.NodeID, qualities []types.SubscribedCodecQuality) {
	if t.dynacastManager != nil {
		t.dynacastManager.NotifySubscriberNodeMaxQuality(nodeID, qualities)
	}
}

func (t *MediaTrack) NotifySubscriptionNode(nodeID livekit.NodeID, codecs []*livekit.SubscribedAudioCodec) {
	if t.dynacastManager != nil {
		t.dynacastManager.NotifySubscriptionNode(nodeID, codecs)
	}
}

func (t *MediaTrack) ClearSubscriberNodes() {
	if t.dynacastManager != nil {
		t.dynacastManager.ClearSubscriberNodes()
	}
}

func (t *MediaTrack) HasSignalCid(cid string) bool {
	if cid != "" {
		ti := t.MediaTrackReceiver.TrackInfoClone()
		for _, c := range ti.Codecs {
			if c.Cid == cid {
				return true
			}
		}
	}
	return false
}

func (t *MediaTrack) HasSdpCid(cid string) bool {
	if cid != "" {
		ti := t.MediaTrackReceiver.TrackInfoClone()
		for _, c := range ti.Codecs {
			if c.Cid == cid || c.SdpCid == cid {
				return true
			}
		}
	}
	return false
}

func (t *MediaTrack) GetMimeTypeForSdpCid(cid string) mime.MimeType {
	if cid != "" {
		ti := t.MediaTrackReceiver.TrackInfoClone()
		for _, c := range ti.Codecs {
			if c.Cid == cid || c.SdpCid == cid {
				return mime.NormalizeMimeType(c.MimeType)
			}
		}
	}
	return mime.MimeTypeUnknown
}

func (t *MediaTrack) GetCidsForMimeType(mimeType mime.MimeType) (string, string) {
	ti := t.MediaTrackReceiver.TrackInfoClone()
	for _, c := range ti.Codecs {
		if mime.NormalizeMimeType(c.MimeType) == mimeType {
			return c.Cid, c.SdpCid
		}
	}
	return "", ""
}

func (t *MediaTrack) ToProto() *livekit.TrackInfo {
	return t.MediaTrackReceiver.TrackInfoClone()
}

// AddReceiver adds a new RTP receiver to the track, returns true when receiver represents a new codec
// and if a receiver was added successfully
func (t *MediaTrack) AddReceiver(receiver *webrtc.RTPReceiver, track sfu.TrackRemote, mid string) (bool, bool) {
	var newCodec bool
	ssrc := uint32(track.SSRC())
	buff, rtcpReader := t.params.BufferFactory.GetBufferPair(ssrc)
	if buff == nil || rtcpReader == nil {
		t.params.Logger.Errorw("could not retrieve buffer pair", nil)
		return newCodec, false
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
					buff.SetSenderReportData(&livekit.RTCPSenderReportState{
						RtpTimestamp: pkt.RTPTime,
						NtpTimestamp: pkt.NTPTime,
						Packets:      pkt.PacketCount,
						Octets:       uint64(pkt.OctetCount),
						At:           mono.UnixNano(),
					})
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
	var regressCodec bool
	mimeType := mime.NormalizeMimeType(track.Codec().MimeType)
	layer := buffer.GetSpatialLayerForRid(mimeType, track.RID(), ti)
	if layer < 0 {
		t.params.Logger.Warnw(
			"AddReceiver failed due to negative layer", nil,
			"rid", track.RID(),
			"layer", layer,
			"ssrc", track.SSRC(),
			"codec", track.Codec(),
			"trackInfo", logger.Proto(ti),
		)
		t.lock.Unlock()
		return newCodec, false
	}

	t.params.Logger.Debugw(
		"AddReceiver",
		"rid", track.RID(),
		"layer", layer,
		"ssrc", track.SSRC(),
		"codec", track.Codec(),
		"trackInfo", logger.Proto(ti),
	)
	wr := t.MediaTrackReceiver.Receiver(mimeType)
	if wr == nil {
		priority := -1
		for idx, c := range ti.Codecs {
			if mime.IsMimeTypeStringEqual(track.Codec().MimeType, c.MimeType) {
				priority = idx
				break
			}
		}
		if priority < 0 {
			switch len(ti.Codecs) {
			case 0:
				// audio track
				t.params.Logger.Warnw(
					"unexpected 0 codecs in track info", nil,
					"mime", mimeType,
					"track", logger.Proto(ti),
				)
				priority = 0
			case 1:
				// older clients or non simulcast-codec, mime type only set later
				if ti.Codecs[0].MimeType == "" {
					priority = 0
				}
			}
		}
		if priority < 0 {
			t.params.Logger.Warnw(
				"could not find codec for webrtc receiver", nil,
				"mime", mimeType,
				"track", logger.Proto(ti),
			)
			t.lock.Unlock()
			return newCodec, false
		}

		newWR := sfu.NewWebRTCReceiver(
			receiver,
			track,
			ti,
			LoggerWithCodecMime(t.params.Logger, mimeType),
			t.params.OnRTCP,
			t.params.VideoConfig.StreamTrackerManager,
			sfu.WithPliThrottleConfig(t.params.PLIThrottleConfig),
			sfu.WithAudioConfig(t.params.AudioConfig),
			sfu.WithLoadBalanceThreshold(20),
			sfu.WithForwardStats(t.params.ForwardStats),
			sfu.WithEnableRTPStreamRestartDetection(t.params.EnableRTPStreamRestartDetection),
		)
		newWR.OnCloseHandler(func() {
			t.MediaTrackReceiver.SetClosing(false)
			t.MediaTrackReceiver.ClearReceiver(mimeType, false)
			if t.MediaTrackReceiver.TryClose() {
				if t.dynacastManager != nil {
					t.dynacastManager.Close()
				}
			}
		})

		// SIMULCAST-CODEC-TODO: these need to be receiver/mime aware, setting it up only for primary now
		statsKey := telemetry.StatsKeyForTrack(
			t.params.ParticipantCountry,
			livekit.StreamType_UPSTREAM,
			t.PublisherID(),
			t.ID(),
			ti.Source,
			ti.Type,
		)
		newWR.OnStatsUpdate(func(_ *sfu.WebRTCReceiver, stat *livekit.AnalyticsStat) {
			// send for only one codec, either primary (priority == 0) OR regressed codec
			t.lock.RLock()
			regressionTargetCodecReceived := t.regressionTargetCodecReceived
			t.lock.RUnlock()
			if priority == 0 || regressionTargetCodecReceived {
				t.params.TelemetryListener.OnTrackStats(statsKey, stat)

				if cs, ok := telemetry.CondenseStat(stat); ok {
					t.params.Reporter.Tx(func(tx roomobs.TrackTx) {
						tx.ReportName(ti.Name)
						tx.ReportKind(roomobs.TrackKindPub)
						tx.ReportType(roomobs.TrackTypeFromProto(ti.Type))
						tx.ReportSource(roomobs.TrackSourceFromProto(ti.Source))
						tx.ReportMime(mime.NormalizeMimeType(ti.MimeType).ReporterType())
						tx.ReportLayer(roomobs.PackTrackLayer(ti.Height, ti.Width))
						tx.ReportDuration(uint16(cs.EndTime.Sub(cs.StartTime).Milliseconds()))
						tx.ReportFrames(uint16(cs.Frames))
						tx.ReportRecvBytes(uint32(cs.Bytes))
						tx.ReportRecvPackets(cs.Packets)
						tx.ReportPacketsLost(cs.PacketsLost)
						tx.ReportScore(stat.Score)
					})
				}
			}
		})

		newWR.OnMaxLayerChange(func(mimeType mime.MimeType, maxLayer int32) {
			// send for only one codec, either primary (priority == 0) OR regressed codec
			t.lock.RLock()
			regressionTargetCodecReceived := t.regressionTargetCodecReceived
			t.lock.RUnlock()
			if priority == 0 || regressionTargetCodecReceived {
				t.MediaTrackReceiver.NotifyMaxLayerChange(mimeType, maxLayer)
			}
		})
		// SIMULCAST-CODEC-TODO END: these need to be receiver/mime aware, setting it up only for primary now

		if t.PrimaryReceiver() == nil {
			// primary codec published, set potential codecs
			potentialCodecs := make([]webrtc.RTPCodecParameters, 0, len(ti.Codecs))
			parameters := receiver.GetParameters()
			for _, c := range ti.Codecs {
				for _, nc := range parameters.Codecs {
					if mime.IsMimeTypeStringEqual(nc.MimeType, c.MimeType) {
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
			if info.Mid == mid && !info.IsRepairStream {
				t.MediaTrackReceiver.SetLayerSsrcsForRid(mimeType, info.StreamID, ssrc, info.RepairSSRC)
			}
		}
		wr = newWR
		newCodec = true

		newWR.AddOnCodecStateChange(func(codec webrtc.RTPCodecParameters, state sfu.ReceiverCodecState) {
			t.MediaTrackReceiver.HandleReceiverCodecChange(newWR, codec, state)
		})

		// update subscriber video layers when video size changes
		newWR.OnVideoSizeChanged(func() {
			if t.params.UpdateTrackInfoByVideoSizeChange {
				t.MediaTrackReceiver.UpdateVideoSize(mimeType, newWR.VideoSizes())
			}

			t.MediaTrackSubscriptions.UpdateVideoLayers()
		})
	}

	if newCodec && t.enableRegression() {
		if mimeType == t.regressionTargetCodec {
			t.params.Logger.Infow("regression target codec received", "codec", mimeType)
			t.regressionTargetCodecReceived = true
			regressCodec = true
		} else if t.regressionTargetCodecReceived {
			regressCodec = true
		}
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
		return newCodec, false
	}

	var expectedBitrate int
	layers := buffer.GetVideoLayersForMimeType(mimeType, ti)
	if layer >= 0 && len(layers) > int(layer) {
		expectedBitrate = int(layers[layer].GetBitrate())
	}
	if err := buff.Bind(receiver.GetParameters(), track.Codec().RTPCodecCapability, expectedBitrate); err != nil {
		t.params.Logger.Warnw(
			"binding buffer failed", err,
			"rid", track.RID(),
			"layer", layer,
			"ssrc", track.SSRC(),
			"newCodec", newCodec,
		)
		buff.Close()
		return newCodec, false
	}

	t.MediaTrackReceiver.SetLayerSsrcsForRid(mimeType, track.RID(), uint32(track.SSRC()), 0)

	if regressCodec {
		for _, c := range ti.Codecs {
			if mime.NormalizeMimeType(c.MimeType) == t.regressionTargetCodec {
				continue
			}

			t.params.Logger.Debugw("suspending codec for codec regression", "codec", c.MimeType)
			if r := t.MediaTrackReceiver.Receiver(mime.NormalizeMimeType(c.MimeType)); r != nil {
				if rtcreceiver, ok := r.(*sfu.WebRTCReceiver); ok {
					rtcreceiver.SetCodecState(sfu.ReceiverCodecStateSuspended)
				}
			}
		}
	}

	buff.OnNotifyRTX(t.MediaTrackReceiver.setLayerRtxInfo)

	// if subscriber request fps before fps calculated, update them after fps updated.
	buff.OnFpsChanged(func() {
		t.MediaTrackSubscriptions.UpdateVideoLayers()
	})

	buff.OnFinalRtpStats(func(stats *livekit.RTPStats) {
		t.params.TelemetryListener.OnTrackPublishRTPStats(
			t.params.ParticipantID(),
			t.ID(),
			mimeType,
			int(layer),
			stats,
		)
	})
	return newCodec, true
}

func (t *MediaTrack) GetConnectionScoreAndQuality() (float32, livekit.ConnectionQuality) {
	receiver := t.ActiveReceiver()
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

func (t *MediaTrack) Restart() {
	t.MediaTrackReceiver.Restart()

	if t.dynacastManager != nil {
		t.dynacastManager.Restart()
	}
}

func (t *MediaTrack) Close(isExpectedToResume bool) {
	t.MediaTrackReceiver.SetClosing(isExpectedToResume)
	if t.dynacastManager != nil {
		t.dynacastManager.Close()
	}
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

// OnTrackSubscribed is called when the track is subscribed by a non-hidden subscriber
// this allows the publisher to know when they should start sending data
func (t *MediaTrack) OnTrackSubscribed() {
	if !t.everSubscribed.Swap(true) && t.params.OnTrackEverSubscribed != nil {
		go t.params.OnTrackEverSubscribed(t.ID())
	}
}

func (t *MediaTrack) enableRegression() bool {
	return t.backupCodecPolicy == livekit.BackupCodecPolicy_REGRESSION ||
		(t.backupCodecPolicy == livekit.BackupCodecPolicy_PREFER_REGRESSION && t.params.ShouldRegressCodec())
}

func (t *MediaTrack) Logger() logger.Logger {
	return t.params.Logger
}

// dynacast.DynacastManagerListtener implementation
var _ dynacast.DynacastManagerListener = (*MediaTrack)(nil)

func (t *MediaTrack) OnDynacastSubscribedMaxQualityChange(
	subscribedQualities []*livekit.SubscribedCodec,
	maxSubscribedQualities []types.SubscribedCodecQuality,
) {
	t.lock.RLock()
	onSubscribedMaxQualityChange := t.onSubscribedMaxQualityChange
	t.lock.RUnlock()

	if onSubscribedMaxQualityChange != nil && !t.IsMuted() {
		_ = onSubscribedMaxQualityChange(
			t.ID(),
			t.ToProto(),
			subscribedQualities,
			maxSubscribedQualities,
		)
	}

	for _, q := range maxSubscribedQualities {
		receiver := t.Receiver(q.CodecMime)
		if receiver != nil {
			receiver.SetMaxExpectedSpatialLayer(
				buffer.GetSpatialLayerForVideoQuality(
					q.CodecMime,
					q.Quality,
					t.MediaTrackReceiver.TrackInfo(),
				),
			)
		}
	}
}

func (t *MediaTrack) OnDynacastSubscribedAudioCodecChange(codecs []*livekit.SubscribedAudioCodec) {
	t.lock.RLock()
	onSubscribedAudioCodecChange := t.onSubscribedAudioCodecChange
	t.lock.RUnlock()

	if onSubscribedAudioCodecChange != nil {
		_ = onSubscribedAudioCodecChange(t.ID(), codecs)
	}
}
