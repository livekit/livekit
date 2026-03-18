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

package sfu

import (
	"strings"
	"sync"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"

	"github.com/livekit/protocol/codecs/mime"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/sfu/connectionquality"
	"github.com/livekit/livekit-server/pkg/sfu/rtpstats"
)

var _ TrackReceiver = (*WebRTCReceiver)(nil)

// WebRTCReceiver receives a media track
type WebRTCReceiver struct {
	*ReceiverBase

	receiver       *webrtc.RTPReceiver
	onCloseHandler func()

	onRTCP func([]rtcp.Packet)

	upTracksMu sync.Mutex
	upTracks   [buffer.DefaultMaxLayerSpatial + 1]TrackRemote

	connectionStats *connectionquality.ConnectionStats
	onStatsUpdate   func(w *WebRTCReceiver, stat *livekit.AnalyticsStat)
}

type ReceiverOpts func(w *WebRTCReceiver) *WebRTCReceiver

// WithPliThrottleConfig indicates minimum time(ms) between sending PLIs
func WithPliThrottleConfig(pliThrottleConfig PLIThrottleConfig) ReceiverOpts {
	return func(w *WebRTCReceiver) *WebRTCReceiver {
		w.ReceiverBase.SetPLIThrottleConfig(pliThrottleConfig)
		return w
	}
}

// WithAudioConfig sets up parameters for active speaker detection
func WithAudioConfig(audioConfig AudioConfig) ReceiverOpts {
	return func(w *WebRTCReceiver) *WebRTCReceiver {
		w.ReceiverBase.SetAudioConfig(audioConfig)
		return w
	}
}

func WithEnableRTPStreamRestartDetection(enable bool) ReceiverOpts {
	return func(w *WebRTCReceiver) *WebRTCReceiver {
		w.ReceiverBase.SetEnableRTPStreamRestartDetection(enable)
		return w
	}
}

// WithLoadBalanceThreshold enables parallelization of packet writes when downTracks exceeds threshold
// Value should be between 3 and 150.
// For a server handling a few large rooms, use a smaller value (required to handle very large (250+ participant) rooms).
// For a server handling many small rooms, use a larger value or disable.
// Set to 0 (disabled) by default.
func WithLoadBalanceThreshold(downTracks int) ReceiverOpts {
	return func(w *WebRTCReceiver) *WebRTCReceiver {
		w.ReceiverBase.SetLBThreshold(downTracks)
		return w
	}
}

func WithForwardStats(forwardStats *ForwardStats) ReceiverOpts {
	return func(w *WebRTCReceiver) *WebRTCReceiver {
		w.ReceiverBase.SetForwardStats(forwardStats)
		return w
	}
}

// NewWebRTCReceiver creates a new webrtc track receiver
func NewWebRTCReceiver(
	receiver *webrtc.RTPReceiver,
	track TrackRemote,
	trackInfo *livekit.TrackInfo,
	logger logger.Logger,
	onRTCP func([]rtcp.Packet),
	streamTrackerManagerConfig StreamTrackerManagerConfig,
	opts ...ReceiverOpts,
) *WebRTCReceiver {
	w := &WebRTCReceiver{
		receiver: receiver,
		onRTCP:   onRTCP,
	}

	w.ReceiverBase = NewReceiverBase(
		ReceiverBaseParams{
			TrackID:                      livekit.TrackID(track.ID()),
			StreamID:                     track.StreamID(),
			Kind:                         track.Kind(),
			Codec:                        track.Codec(),
			HeaderExtensions:             receiver.GetParameters().HeaderExtensions,
			Logger:                       logger,
			StreamTrackerManagerConfig:   streamTrackerManagerConfig,
			StreamTrackerManagerListener: w,
			IsSelfClosing:                true,
			OnClosed:                     w.onClosed,
		},
		trackInfo,
		ReceiverCodecStateNormal,
	)

	for _, opt := range opts {
		w = opt(w)
	}

	w.connectionStats = connectionquality.NewConnectionStats(connectionquality.ConnectionStatsParams{
		ReceiverProvider: w,
		Logger:           logger.WithValues("direction", "up"),
	})
	w.connectionStats.OnStatsUpdate(func(_cs *connectionquality.ConnectionStats, stat *livekit.AnalyticsStat) {
		if w.onStatsUpdate != nil {
			w.onStatsUpdate(w, stat)
		}
	})
	codec := track.Codec()
	w.connectionStats.Start(
		mime.NormalizeMimeType(codec.MimeType),
		// TODO: technically not correct to declare FEC on when RED. Need the primary codec's fmtp line to check.
		mime.IsMimeTypeStringRED(codec.MimeType) || strings.Contains(strings.ToLower(codec.SDPFmtpLine), "useinbandfec=1"),
	)

	return w
}

func (w *WebRTCReceiver) OnStatsUpdate(fn func(w *WebRTCReceiver, stat *livekit.AnalyticsStat)) {
	w.onStatsUpdate = fn
}

func (w *WebRTCReceiver) GetConnectionScoreAndQuality() (float32, livekit.ConnectionQuality) {
	return w.connectionStats.GetScoreAndQuality()
}

func (w *WebRTCReceiver) ssrc(layer int) uint32 {
	if track := w.upTracks[layer]; track != nil {
		return uint32(track.SSRC())
	}
	return 0
}

func (w *WebRTCReceiver) AddUpTrack(track TrackRemote, buff *buffer.Buffer) error {
	if w.isClosed.Load() {
		return ErrReceiverClosed
	}

	layer := int32(0)
	if w.Kind() == webrtc.RTPCodecTypeVideo && w.videoLayerMode != livekit.VideoLayer_MULTIPLE_SPATIAL_LAYERS_PER_STREAM {
		layer = buffer.GetSpatialLayerForRid(w.Mime(), track.RID(), w.ReceiverBase.TrackInfo())
	}
	if layer < 0 {
		w.ReceiverBase.Logger().Warnw(
			"invalid layer", nil,
			"rid", track.RID(),
			"trackInfo", logger.Proto(w.ReceiverBase.TrackInfo()),
		)
		return ErrInvalidLayer
	}

	w.upTracksMu.Lock()
	if w.upTracks[layer] != nil {
		w.upTracksMu.Unlock()
		return ErrDuplicateLayer
	}
	w.upTracks[layer] = track
	w.upTracksMu.Unlock()

	w.ReceiverBase.AddBuffer(buff, layer)
	buff.OnRtcpFeedback(w.sendRTCP)
	w.ReceiverBase.StartBuffer(buff, layer)
	return nil
}

func (w *WebRTCReceiver) SetUpTrackPaused(paused bool) {
	w.ReceiverBase.SetUpTrackPaused(paused)

	w.connectionStats.UpdateMute(paused)
}

func (w *WebRTCReceiver) notifyMaxExpectedLayer(layer int32) {
	ti := w.TrackInfo()
	if ti == nil {
		return
	}

	if w.Kind() == webrtc.RTPCodecTypeAudio || ti.Source == livekit.TrackSource_SCREEN_SHARE {
		// screen share tracks have highly variable bitrate, do not use bit rate based quality for those
		return
	}

	expectedBitrate := int64(0)
	for _, vl := range buffer.GetVideoLayersForMimeType(w.Mime(), ti) {
		if vl.SpatialLayer <= layer {
			expectedBitrate += int64(vl.Bitrate)
		}
	}

	w.connectionStats.AddBitrateTransition(expectedBitrate)
}

func (w *WebRTCReceiver) SetMaxExpectedSpatialLayer(layer int32) {
	w.ReceiverBase.SetMaxExpectedSpatialLayer(layer)

	w.notifyMaxExpectedLayer(layer)

	if layer == buffer.InvalidLayerSpatial {
		w.connectionStats.UpdateLayerMute(true)
	} else {
		w.connectionStats.UpdateLayerMute(false)
		w.connectionStats.AddLayerTransition(w.ReceiverBase.StreamTrackerManager().DistanceToDesired())
	}
}

// StreamTrackerManagerListener.OnAvailableLayersChanged
func (w *WebRTCReceiver) OnAvailableLayersChanged() {
	w.connectionStats.AddLayerTransition(w.ReceiverBase.StreamTrackerManager().DistanceToDesired())
}

// StreamTrackerManagerListener.OnBitrateAvailabilityChanged
func (w *WebRTCReceiver) OnBitrateAvailabilityChanged() {
}

// StreamTrackerManagerListener.OnMaxPublishedLayerChanged
func (w *WebRTCReceiver) OnMaxPublishedLayerChanged(maxPublishedLayer int32) {
	w.notifyMaxExpectedLayer(maxPublishedLayer)
	w.connectionStats.AddLayerTransition(w.ReceiverBase.StreamTrackerManager().DistanceToDesired())
}

// StreamTrackerManagerListener.OnMaxTemporalLayerSeenChanged
func (w *WebRTCReceiver) OnMaxTemporalLayerSeenChanged(maxTemporalLayerSeen int32) {
	w.connectionStats.AddLayerTransition(w.ReceiverBase.StreamTrackerManager().DistanceToDesired())
}

// StreamTrackerManagerListener.OnMaxAvailableLayerChanged
func (w *WebRTCReceiver) OnMaxAvailableLayerChanged(maxAvailableLayer int32) {
}

// StreamTrackerManagerListener.OnBitrateReport
func (w *WebRTCReceiver) OnBitrateReport(availableLayers []int32, bitrates Bitrates) {
	w.connectionStats.AddLayerTransition(w.ReceiverBase.StreamTrackerManager().DistanceToDesired())
}

// OnCloseHandler method to be called on remote track removed
func (w *WebRTCReceiver) OnCloseHandler(fn func()) {
	w.onCloseHandler = fn
}

func (w *WebRTCReceiver) sendRTCP(packets []rtcp.Packet) {
	if packets == nil || w.isClosed.Load() {
		return
	}

	if w.onRTCP != nil {
		w.onRTCP(packets)
	}
}

func (w *WebRTCReceiver) GetDeltaStats() map[uint32]*buffer.StreamStatsWithLayers {
	buffers := w.ReceiverBase.GetAllBuffers()
	deltaStats := make(map[uint32]*buffer.StreamStatsWithLayers, len(buffers))
	for layer, buff := range buffers {
		if buff == nil {
			continue
		}

		sswl := buff.GetDeltaStats()
		if sswl == nil {
			continue
		}

		// patch buffer stats with correct layer
		patched := make(map[int32]*rtpstats.RTPDeltaInfo, 1)
		patched[int32(layer)] = sswl.Layers[0]
		sswl.Layers = patched

		deltaStats[w.ssrc(layer)] = sswl
	}

	return deltaStats
}

func (w *WebRTCReceiver) GetLastSenderReportTime() time.Time {
	buffers := w.ReceiverBase.GetAllBuffers()
	latestSRTime := time.Time{}
	for _, buff := range buffers {
		if buff == nil {
			continue
		}

		srAt := buff.GetLastSenderReportTime()
		if srAt.After(latestSRTime) {
			latestSRTime = srAt
		}
	}

	return latestSRTime
}

func (w *WebRTCReceiver) onClosed() {
	w.connectionStats.Close()

	if w.onCloseHandler != nil {
		w.onCloseHandler()
	}
}

func (w *WebRTCReceiver) DebugInfo() map[string]any {
	info := w.ReceiverBase.DebugInfo()

	w.upTracksMu.Lock()
	upTrackInfo := make([]map[string]any, 0, len(w.upTracks))
	for layer, ut := range w.upTracks {
		if ut != nil {
			upTrackInfo = append(upTrackInfo, map[string]any{
				"Layer": layer,
				"SSRC":  ut.SSRC(),
				"Msid":  ut.Msid(),
				"RID":   ut.RID(),
			})
		}
	}
	w.upTracksMu.Unlock()
	info["UpTracks"] = upTrackInfo

	return info
}

// -----------------------------------------------------------
