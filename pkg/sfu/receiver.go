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
	"errors"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
	"go.uber.org/atomic"

	"github.com/livekit/mediatransportutil/pkg/bucket"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils"

	"github.com/livekit/livekit-server/pkg/sfu/audio"
	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/sfu/connectionquality"
	dd "github.com/livekit/livekit-server/pkg/sfu/rtpextension/dependencydescriptor"
	"github.com/livekit/livekit-server/pkg/sfu/rtpstats"
	"github.com/livekit/livekit-server/pkg/sfu/streamtracker"
)

var (
	ErrReceiverClosed        = errors.New("receiver closed")
	ErrDownTrackAlreadyExist = errors.New("DownTrack already exist")
	ErrBufferNotFound        = errors.New("buffer not found")
	ErrDuplicateLayer        = errors.New("duplicate layer")
)

// --------------------------------------

type PLIThrottleConfig struct {
	LowQuality  time.Duration `yaml:"low_quality,omitempty"`
	MidQuality  time.Duration `yaml:"mid_quality,omitempty"`
	HighQuality time.Duration `yaml:"high_quality,omitempty"`
}

var (
	DefaultPLIThrottleConfig = PLIThrottleConfig{
		LowQuality:  500 * time.Millisecond,
		MidQuality:  time.Second,
		HighQuality: time.Second,
	}
)

// --------------------------------------

type AudioConfig struct {
	audio.AudioLevelConfig `yaml:",inline"`

	// enable red encoding downtrack for opus only audio up track
	ActiveREDEncoding bool `yaml:"active_red_encoding,omitempty"`
	// enable proxying weakest subscriber loss to publisher in RTCP Receiver Report
	EnableLossProxying bool `yaml:"enable_loss_proxying,omitempty"`
}

var (
	DefaultAudioConfig = AudioConfig{
		AudioLevelConfig: audio.DefaultAudioLevelConfig,
	}
)

// --------------------------------------

type AudioLevelHandle func(level uint8, duration uint32)

type Bitrates [buffer.DefaultMaxLayerSpatial + 1][buffer.DefaultMaxLayerTemporal + 1]int64

// TrackReceiver defines an interface receive media from remote peer
type TrackReceiver interface {
	TrackID() livekit.TrackID
	StreamID() string
	Codec() webrtc.RTPCodecParameters
	HeaderExtensions() []webrtc.RTPHeaderExtensionParameter
	IsClosed() bool

	ReadRTP(buf []byte, layer uint8, esn uint64) (int, error)
	GetLayeredBitrate() ([]int32, Bitrates)

	GetAudioLevel() (float64, bool)

	SendPLI(layer int32, force bool)

	SetUpTrackPaused(paused bool)
	SetMaxExpectedSpatialLayer(layer int32)

	AddDownTrack(track TrackSender) error
	DeleteDownTrack(participantID livekit.ParticipantID)

	DebugInfo() map[string]interface{}

	TrackInfo() *livekit.TrackInfo
	UpdateTrackInfo(ti *livekit.TrackInfo)

	// Get primary receiver if this receiver represents a RED codec; otherwise it will return itself
	GetPrimaryReceiverForRed() TrackReceiver

	// Get red receiver for primary codec, used by forward red encodings for opus only codec
	GetRedReceiver() TrackReceiver

	GetTemporalLayerFpsForSpatial(layer int32) []float32

	GetTrackStats() *livekit.RTPStats

	// AddOnReady adds a function to be called when the receiver is ready, the callback
	// could be called immediately if the receiver is ready when the callback is added
	AddOnReady(func())
}

type redPktWriteFunc func(pkt *buffer.ExtPacket, spatialLayer int32) int

// WebRTCReceiver receives a media track
type WebRTCReceiver struct {
	logger logger.Logger

	pliThrottleConfig PLIThrottleConfig
	audioConfig       AudioConfig

	trackID        livekit.TrackID
	streamID       string
	kind           webrtc.RTPCodecType
	receiver       *webrtc.RTPReceiver
	codec          webrtc.RTPCodecParameters
	isSVC          bool
	isRED          bool
	onCloseHandler func()
	closeOnce      sync.Once
	closed         atomic.Bool
	useTrackers    bool
	trackInfo      atomic.Pointer[livekit.TrackInfo]

	onRTCP func([]rtcp.Packet)

	bufferMu sync.RWMutex
	buffers  [buffer.DefaultMaxLayerSpatial + 1]*buffer.Buffer
	upTracks [buffer.DefaultMaxLayerSpatial + 1]TrackRemote
	rtt      uint32

	lbThreshold int

	streamTrackerManager *StreamTrackerManager

	downTrackSpreader *DownTrackSpreader

	connectionStats *connectionquality.ConnectionStats

	onStatsUpdate    func(w *WebRTCReceiver, stat *livekit.AnalyticsStat)
	onMaxLayerChange func(maxLayer int32)

	primaryReceiver atomic.Pointer[RedPrimaryReceiver]
	redReceiver     atomic.Pointer[RedReceiver]
	redPktWriter    atomic.Value // redPktWriteFunc

	forwardStats *ForwardStats
}

type ReceiverOpts func(w *WebRTCReceiver) *WebRTCReceiver

// WithPliThrottleConfig indicates minimum time(ms) between sending PLIs
func WithPliThrottleConfig(pliThrottleConfig PLIThrottleConfig) ReceiverOpts {
	return func(w *WebRTCReceiver) *WebRTCReceiver {
		w.pliThrottleConfig = pliThrottleConfig
		return w
	}
}

// WithAudioConfig sets up parameters for active speaker detection
func WithAudioConfig(audioConfig AudioConfig) ReceiverOpts {
	return func(w *WebRTCReceiver) *WebRTCReceiver {
		w.audioConfig = audioConfig
		return w
	}
}

// WithStreamTrackers enables StreamTracker use for simulcast
func WithStreamTrackers() ReceiverOpts {
	return func(w *WebRTCReceiver) *WebRTCReceiver {
		w.useTrackers = true
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
		w.lbThreshold = downTracks
		return w
	}
}

func WithForwardStats(forwardStats *ForwardStats) ReceiverOpts {
	return func(w *WebRTCReceiver) *WebRTCReceiver {
		w.forwardStats = forwardStats
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
		logger:   logger,
		receiver: receiver,
		trackID:  livekit.TrackID(track.ID()),
		streamID: track.StreamID(),
		codec:    track.Codec(),
		kind:     track.Kind(),
		onRTCP:   onRTCP,
		isSVC:    buffer.IsSvcCodec(track.Codec().MimeType),
		isRED:    buffer.IsRedCodec(track.Codec().MimeType),
	}

	for _, opt := range opts {
		w = opt(w)
	}
	w.trackInfo.Store(utils.CloneProto(trackInfo))

	w.downTrackSpreader = NewDownTrackSpreader(DownTrackSpreaderParams{
		Threshold: w.lbThreshold,
		Logger:    logger,
	})

	w.connectionStats = connectionquality.NewConnectionStats(connectionquality.ConnectionStatsParams{
		ReceiverProvider: w,
		Logger:           w.logger.WithValues("direction", "up"),
	})
	w.connectionStats.OnStatsUpdate(func(_cs *connectionquality.ConnectionStats, stat *livekit.AnalyticsStat) {
		if w.onStatsUpdate != nil {
			w.onStatsUpdate(w, stat)
		}
	})
	w.connectionStats.Start(
		w.codec.MimeType,
		// TODO: technically not correct to declare FEC on when RED. Need the primary codec's fmtp line to check.
		strings.EqualFold(w.codec.MimeType, MimeTypeAudioRed) || strings.Contains(strings.ToLower(w.codec.SDPFmtpLine), "useinbandfec=1"),
	)

	w.streamTrackerManager = NewStreamTrackerManager(logger, trackInfo, w.isSVC, w.codec.ClockRate, streamTrackerManagerConfig)
	w.streamTrackerManager.SetListener(w)
	// SVC-TODO: Handle DD for non-SVC cases???
	if w.isSVC {
		for _, ext := range receiver.GetParameters().HeaderExtensions {
			if ext.URI == dd.ExtensionURI {
				w.streamTrackerManager.AddDependencyDescriptorTrackers()
				break
			}
		}
	}

	return w
}

func (w *WebRTCReceiver) TrackInfo() *livekit.TrackInfo {
	return w.trackInfo.Load()
}

func (w *WebRTCReceiver) UpdateTrackInfo(ti *livekit.TrackInfo) {
	w.trackInfo.Store(utils.CloneProto(ti))
	w.streamTrackerManager.UpdateTrackInfo(ti)
}

func (w *WebRTCReceiver) OnStatsUpdate(fn func(w *WebRTCReceiver, stat *livekit.AnalyticsStat)) {
	w.onStatsUpdate = fn
}

func (w *WebRTCReceiver) OnMaxLayerChange(fn func(maxLayer int32)) {
	w.bufferMu.Lock()
	w.onMaxLayerChange = fn
	w.bufferMu.Unlock()
}

func (w *WebRTCReceiver) getOnMaxLayerChange() func(maxLayer int32) {
	w.bufferMu.RLock()
	defer w.bufferMu.RUnlock()

	return w.onMaxLayerChange
}

func (w *WebRTCReceiver) GetConnectionScoreAndQuality() (float32, livekit.ConnectionQuality) {
	return w.connectionStats.GetScoreAndQuality()
}

func (w *WebRTCReceiver) IsClosed() bool {
	return w.closed.Load()
}

func (w *WebRTCReceiver) SetRTT(rtt uint32) {
	w.bufferMu.Lock()
	if w.rtt == rtt {
		w.bufferMu.Unlock()
		return
	}

	w.rtt = rtt
	buffers := w.buffers
	w.bufferMu.Unlock()

	for _, buff := range buffers {
		if buff == nil {
			continue
		}

		buff.SetRTT(rtt)
	}
}

func (w *WebRTCReceiver) StreamID() string {
	return w.streamID
}

func (w *WebRTCReceiver) TrackID() livekit.TrackID {
	return w.trackID
}

func (w *WebRTCReceiver) ssrc(layer int) uint32 {
	if track := w.upTracks[layer]; track != nil {
		return uint32(track.SSRC())
	}
	return 0
}

func (w *WebRTCReceiver) Codec() webrtc.RTPCodecParameters {
	return w.codec
}

func (w *WebRTCReceiver) HeaderExtensions() []webrtc.RTPHeaderExtensionParameter {
	return w.receiver.GetParameters().HeaderExtensions
}

func (w *WebRTCReceiver) Kind() webrtc.RTPCodecType {
	return w.kind
}

func (w *WebRTCReceiver) AddUpTrack(track TrackRemote, buff *buffer.Buffer) error {
	if w.closed.Load() {
		return ErrReceiverClosed
	}

	layer := int32(0)
	if w.Kind() == webrtc.RTPCodecTypeVideo && !w.isSVC {
		layer = buffer.RidToSpatialLayer(track.RID(), w.trackInfo.Load())
	}
	buff.SetLogger(w.logger.WithValues("layer", layer))
	buff.SetAudioLevelParams(audio.AudioLevelParams{
		Config: w.audioConfig.AudioLevelConfig,
	})
	buff.SetAudioLossProxying(w.audioConfig.EnableLossProxying)
	buff.OnRtcpFeedback(w.sendRTCP)
	buff.OnRtcpSenderReport(func() {
		srData := buff.GetSenderReportData()
		w.downTrackSpreader.Broadcast(func(dt TrackSender) {
			_ = dt.HandleRTCPSenderReportData(w.codec.PayloadType, w.isSVC, layer, srData)
		})
	})

	var duration time.Duration
	switch layer {
	case 2:
		duration = w.pliThrottleConfig.HighQuality
	case 1:
		duration = w.pliThrottleConfig.MidQuality
	case 0:
		duration = w.pliThrottleConfig.LowQuality
	default:
		duration = w.pliThrottleConfig.MidQuality
	}
	if duration != 0 {
		buff.SetPLIThrottle(duration.Nanoseconds())
	}

	w.bufferMu.Lock()
	if w.upTracks[layer] != nil {
		w.bufferMu.Unlock()
		return ErrDuplicateLayer
	}
	w.upTracks[layer] = track
	w.buffers[layer] = buff
	rtt := w.rtt
	w.bufferMu.Unlock()

	buff.SetRTT(rtt)
	buff.SetPaused(w.streamTrackerManager.IsPaused())

	if w.Kind() == webrtc.RTPCodecTypeVideo && w.useTrackers {
		w.streamTrackerManager.AddTracker(layer)
	}

	go w.forwardRTP(layer, buff)
	return nil
}

// SetUpTrackPaused indicates upstream will not be sending any data.
// this will reflect the "muted" status and will pause streamtracker to ensure we don't turn off
// the layer
func (w *WebRTCReceiver) SetUpTrackPaused(paused bool) {
	w.streamTrackerManager.SetPaused(paused)

	w.bufferMu.RLock()
	for _, buff := range w.buffers {
		if buff == nil {
			continue
		}

		buff.SetPaused(paused)
	}
	w.bufferMu.RUnlock()

	w.connectionStats.UpdateMute(paused)
}

func (w *WebRTCReceiver) AddDownTrack(track TrackSender) error {
	if w.closed.Load() {
		return ErrReceiverClosed
	}

	if w.downTrackSpreader.HasDownTrack(track.SubscriberID()) {
		w.logger.Infow("subscriberID already exists, replacing downtrack", "subscriberID", track.SubscriberID())
	}

	track.UpTrackMaxPublishedLayerChange(w.streamTrackerManager.GetMaxPublishedLayer())
	track.UpTrackMaxTemporalLayerSeenChange(w.streamTrackerManager.GetMaxTemporalLayerSeen())

	w.downTrackSpreader.Store(track)
	w.logger.Debugw("downtrack added", "subscriberID", track.SubscriberID())
	return nil
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
	for _, vl := range ti.Layers {
		l := buffer.VideoQualityToSpatialLayer(vl.Quality, ti)
		if l <= layer {
			expectedBitrate += int64(vl.Bitrate)
		}
	}

	w.connectionStats.AddBitrateTransition(expectedBitrate)
}

func (w *WebRTCReceiver) SetMaxExpectedSpatialLayer(layer int32) {
	w.streamTrackerManager.SetMaxExpectedSpatialLayer(layer)
	w.notifyMaxExpectedLayer(layer)

	if layer == buffer.InvalidLayerSpatial {
		w.connectionStats.UpdateLayerMute(true)
	} else {
		w.connectionStats.UpdateLayerMute(false)
		w.connectionStats.AddLayerTransition(w.streamTrackerManager.DistanceToDesired())
	}
}

// StreamTrackerManagerListener.OnAvailableLayersChanged
func (w *WebRTCReceiver) OnAvailableLayersChanged() {
	w.downTrackSpreader.Broadcast(func(dt TrackSender) {
		dt.UpTrackLayersChange()
	})

	w.connectionStats.AddLayerTransition(w.streamTrackerManager.DistanceToDesired())
}

// StreamTrackerManagerListener.OnBitrateAvailabilityChanged
func (w *WebRTCReceiver) OnBitrateAvailabilityChanged() {
	w.downTrackSpreader.Broadcast(func(dt TrackSender) {
		dt.UpTrackBitrateAvailabilityChange()
	})
}

// StreamTrackerManagerListener.OnMaxPublishedLayerChanged
func (w *WebRTCReceiver) OnMaxPublishedLayerChanged(maxPublishedLayer int32) {
	w.downTrackSpreader.Broadcast(func(dt TrackSender) {
		dt.UpTrackMaxPublishedLayerChange(maxPublishedLayer)
	})

	w.notifyMaxExpectedLayer(maxPublishedLayer)
	w.connectionStats.AddLayerTransition(w.streamTrackerManager.DistanceToDesired())
}

// StreamTrackerManagerListener.OnMaxTemporalLayerSeenChanged
func (w *WebRTCReceiver) OnMaxTemporalLayerSeenChanged(maxTemporalLayerSeen int32) {
	w.downTrackSpreader.Broadcast(func(dt TrackSender) {
		dt.UpTrackMaxTemporalLayerSeenChange(maxTemporalLayerSeen)
	})

	w.connectionStats.AddLayerTransition(w.streamTrackerManager.DistanceToDesired())
}

// StreamTrackerManagerListener.OnMaxAvailableLayerChanged
func (w *WebRTCReceiver) OnMaxAvailableLayerChanged(maxAvailableLayer int32) {
	if onMaxLayerChange := w.getOnMaxLayerChange(); onMaxLayerChange != nil {
		onMaxLayerChange(maxAvailableLayer)
	}
}

// StreamTrackerManagerListener.OnBitrateReport
func (w *WebRTCReceiver) OnBitrateReport(availableLayers []int32, bitrates Bitrates) {
	w.downTrackSpreader.Broadcast(func(dt TrackSender) {
		dt.UpTrackBitrateReport(availableLayers, bitrates)
	})

	w.connectionStats.AddLayerTransition(w.streamTrackerManager.DistanceToDesired())
}

func (w *WebRTCReceiver) GetLayeredBitrate() ([]int32, Bitrates) {
	return w.streamTrackerManager.GetLayeredBitrate()
}

// OnCloseHandler method to be called on remote tracked removed
func (w *WebRTCReceiver) OnCloseHandler(fn func()) {
	w.onCloseHandler = fn
}

// DeleteDownTrack removes a DownTrack from a Receiver
func (w *WebRTCReceiver) DeleteDownTrack(subscriberID livekit.ParticipantID) {
	if w.closed.Load() {
		return
	}

	w.downTrackSpreader.Free(subscriberID)
	w.logger.Debugw("downtrack deleted", "subscriberID", subscriberID)
}

func (w *WebRTCReceiver) sendRTCP(packets []rtcp.Packet) {
	if packets == nil || w.closed.Load() {
		return
	}

	if w.onRTCP != nil {
		w.onRTCP(packets)
	}
}

func (w *WebRTCReceiver) SendPLI(layer int32, force bool) {
	// SVC-TODO :  should send LRR (Layer Refresh Request) instead of PLI
	buff := w.getBuffer(layer)
	if buff == nil {
		return
	}

	buff.SendPLI(force)
}

func (w *WebRTCReceiver) getBuffer(layer int32) *buffer.Buffer {
	w.bufferMu.RLock()
	defer w.bufferMu.RUnlock()

	return w.getBufferLocked(layer)
}

func (w *WebRTCReceiver) getBufferLocked(layer int32) *buffer.Buffer {
	// for svc codecs, use layer = 0 always.
	// spatial layers are in-built and handled by single buffer
	if w.isSVC {
		layer = 0
	}

	if layer < 0 || int(layer) >= len(w.buffers) {
		return nil
	}

	return w.buffers[layer]
}

func (w *WebRTCReceiver) ReadRTP(buf []byte, layer uint8, esn uint64) (int, error) {
	b := w.getBuffer(int32(layer))
	if b == nil {
		return 0, ErrBufferNotFound
	}

	return b.GetPacket(buf, esn)
}

func (w *WebRTCReceiver) GetTrackStats() *livekit.RTPStats {
	w.bufferMu.RLock()
	defer w.bufferMu.RUnlock()

	stats := make([]*livekit.RTPStats, 0, len(w.buffers))
	for _, buff := range w.buffers {
		if buff == nil {
			continue
		}

		sswl := buff.GetStats()
		if sswl == nil {
			continue
		}

		stats = append(stats, sswl)
	}

	return rtpstats.AggregateRTPStats(stats)
}

func (w *WebRTCReceiver) GetAudioLevel() (float64, bool) {
	if w.Kind() == webrtc.RTPCodecTypeVideo {
		return 0, false
	}

	w.bufferMu.RLock()
	defer w.bufferMu.RUnlock()

	for _, buff := range w.buffers {
		if buff == nil {
			continue
		}

		return buff.GetAudioLevel()
	}

	return 0, false
}

func (w *WebRTCReceiver) GetDeltaStats() map[uint32]*buffer.StreamStatsWithLayers {
	w.bufferMu.RLock()
	defer w.bufferMu.RUnlock()

	deltaStats := make(map[uint32]*buffer.StreamStatsWithLayers, len(w.buffers))

	for layer, buff := range w.buffers {
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
	w.bufferMu.RLock()
	defer w.bufferMu.RUnlock()

	latestSRTime := time.Time{}
	for _, buff := range w.buffers {
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

func (w *WebRTCReceiver) forwardRTP(layer int32, buff *buffer.Buffer) {
	defer func() {
		w.closeOnce.Do(func() {
			w.closed.Store(true)
			w.closeTracks()
			if pr := w.primaryReceiver.Load(); pr != nil {
				pr.Close()
			}
			if pr := w.redReceiver.Load(); pr != nil {
				pr.Close()
			}
		})

		w.streamTrackerManager.RemoveTracker(layer)
		if w.isSVC {
			w.streamTrackerManager.RemoveAllTrackers()
		}
	}()

	var spatialTrackers [buffer.DefaultMaxLayerSpatial + 1]streamtracker.StreamTrackerWorker
	if layer < 0 || int(layer) >= len(spatialTrackers) {
		w.logger.Errorw("invalid layer", nil, "layer", layer)
		return
	}
	spatialTrackers[layer] = w.streamTrackerManager.GetTracker(layer)

	pktBuf := make([]byte, bucket.MaxPktSize)
	for {
		pkt, err := buff.ReadExtended(pktBuf)
		if err == io.EOF {
			return
		}

		spatialLayer := layer
		if pkt.Spatial >= 0 {
			// svc packet, take spatial layer info from packet
			spatialLayer = pkt.Spatial
		}

		writeCount := w.downTrackSpreader.Broadcast(func(dt TrackSender) {
			_ = dt.WriteRTP(pkt, spatialLayer)
		})

		if f := w.redPktWriter.Load(); f != nil {
			writeCount += f.(redPktWriteFunc)(pkt, spatialLayer)
		}

		// track delay/jitter
		if writeCount > 0 && w.forwardStats != nil {
			w.forwardStats.Update(pkt.Arrival, time.Now().UnixNano())
		}

		// track video layers
		if w.Kind() == webrtc.RTPCodecTypeVideo {
			if spatialTrackers[spatialLayer] == nil {
				spatialTrackers[spatialLayer] = w.streamTrackerManager.GetTracker(spatialLayer)
				if spatialTrackers[spatialLayer] == nil {
					spatialTrackers[spatialLayer] = w.streamTrackerManager.AddTracker(spatialLayer)
				}
			}
			if spatialTrackers[spatialLayer] != nil {
				spatialTrackers[spatialLayer].Observe(
					pkt.Temporal,
					len(pkt.RawPacket),
					len(pkt.Packet.Payload),
					pkt.Packet.Marker,
					pkt.Packet.Timestamp,
					pkt.DependencyDescriptor,
				)
			}
		}
	}
}

// closeTracks close all tracks from Receiver
func (w *WebRTCReceiver) closeTracks() {
	w.connectionStats.Close()
	w.streamTrackerManager.Close()

	closeTrackSenders(w.downTrackSpreader.ResetAndGetDownTracks())

	if w.onCloseHandler != nil {
		w.onCloseHandler()
	}
}

func (w *WebRTCReceiver) DebugInfo() map[string]interface{} {
	isSimulcast := !w.isSVC
	if ti := w.trackInfo.Load(); ti != nil {
		isSimulcast = isSimulcast && len(ti.Layers) > 1
	}
	info := map[string]interface{}{
		"SVC":       w.isSVC,
		"Simulcast": isSimulcast,
	}

	w.bufferMu.RLock()
	upTrackInfo := make([]map[string]interface{}, 0, len(w.upTracks))
	for layer, ut := range w.upTracks {
		if ut != nil {
			upTrackInfo = append(upTrackInfo, map[string]interface{}{
				"Layer": layer,
				"SSRC":  ut.SSRC(),
				"Msid":  ut.Msid(),
				"RID":   ut.RID(),
			})
		}
	}
	w.bufferMu.RUnlock()
	info["UpTracks"] = upTrackInfo

	return info
}

func (w *WebRTCReceiver) GetPrimaryReceiverForRed() TrackReceiver {
	if !w.isRED || w.closed.Load() {
		return w
	}

	if w.primaryReceiver.Load() == nil {
		pr := NewRedPrimaryReceiver(w, DownTrackSpreaderParams{
			Threshold: w.lbThreshold,
			Logger:    w.logger,
		})
		if w.primaryReceiver.CompareAndSwap(nil, pr) {
			w.redPktWriter.Store(redPktWriteFunc(pr.ForwardRTP))
		}
	}
	return w.primaryReceiver.Load()
}

func (w *WebRTCReceiver) GetRedReceiver() TrackReceiver {
	if w.isRED || w.closed.Load() {
		return w
	}

	if w.redReceiver.Load() == nil {
		pr := NewRedReceiver(w, DownTrackSpreaderParams{
			Threshold: w.lbThreshold,
			Logger:    w.logger,
		})
		if w.redReceiver.CompareAndSwap(nil, pr) {
			w.redPktWriter.Store(redPktWriteFunc(pr.ForwardRTP))
		}
	}
	return w.redReceiver.Load()
}

func (w *WebRTCReceiver) GetTemporalLayerFpsForSpatial(layer int32) []float32 {
	b := w.getBuffer(layer)
	if b == nil {
		return nil
	}

	if !w.isSVC {
		return b.GetTemporalLayerFpsForSpatial(0)
	}

	return b.GetTemporalLayerFpsForSpatial(layer)
}

func (w *WebRTCReceiver) AddOnReady(fn func()) {
	// webRTCReceiver is always ready after created
	fn()
}

// -----------------------------------------------------------

// closes all track senders in parallel, returns when all are closed
func closeTrackSenders(senders []TrackSender) {
	wg := sync.WaitGroup{}
	for _, dt := range senders {
		dt := dt
		wg.Add(1)
		go func() {
			defer wg.Done()
			dt.Close()
		}()
	}
	wg.Wait()
}
