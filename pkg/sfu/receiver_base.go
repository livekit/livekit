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
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
	"go.uber.org/atomic"

	"github.com/livekit/mediatransportutil/pkg/bucket"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils"
	"github.com/livekit/protocol/utils/mono"

	"github.com/livekit/livekit-server/pkg/sfu"
	"github.com/livekit/livekit-server/pkg/sfu/audio"
	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/sfu/mime"
	"github.com/livekit/livekit-server/pkg/sfu/rtpstats"
	"github.com/livekit/livekit-server/pkg/sfu/streamtracker"
	sfuutils "github.com/livekit/livekit-server/pkg/sfu/utils"
)

var (
	ErrReceiverClosed        = errors.New("receiver closed")
	ErrDownTrackAlreadyExist = errors.New("DownTrack already exist")
	ErrDuplicateLayer        = errors.New("duplicate layer")
	ErrInvalidLayer          = errors.New("invalid layer")
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

type ReceiverCodecState int

const (
	ReceiverCodecStateNormal ReceiverCodecState = iota
	ReceiverCodecStateSuspended
	ReceiverCodecStateInvalid
)

// TrackReceiver defines an interface receive media from remote peer
type TrackReceiver interface {
	TrackID() livekit.TrackID
	StreamID() string

	// returns the initial codec of the receiver, it is determined by the track's codec
	// and will not change if the codec changes during the session (publisher changes codec)
	Codec() webrtc.RTPCodecParameters
	Mime() mime.MimeType
	VideoLayerMode() livekit.VideoLayer_Mode
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
	GetDownTracks() []TrackSender

	DebugInfo() map[string]any

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

	AddOnCodecStateChange(func(webrtc.RTPCodecParameters, ReceiverCodecState))
	CodecState() ReceiverCodecState

	// VideoSizes returns the video size parsed from rtp packet for each spatial layer.
	VideoSizes() []buffer.VideoSize
}

type REDTransformer interface {
	ForwardRTP(pkt *buffer.ExtPacket, spatialLayer int32) int32
	ForwardRTCPSenderReport(
		payloadType webrtc.PayloadType,
		layer int32,
		publisherSRData *livekit.RTCPSenderReportState,
	)
	ResyncDownTracks()
	OnStreamRestart()
	CanClose() bool
	Close()
}

type ReceiverBase struct {
	logger logger.Logger

	pliThrottleConfig               PLIThrottleConfig
	audioConfig                     AudioConfig
	enableRTPStreamRestartDetection bool
	lbThreshold                     int // RAJA-TODO: this needs to be passed in properly
	forwardStats                    *ForwardStats

	trackID          livekit.TrackID
	streamID         string
	kind             webrtc.RTPCodecType
	codec            webrtc.RTPCodecParameters
	headerExtensions []webrtc.RTPHeaderExtensionParameter

	codecState         ReceiverCodecState
	codecStateLock     sync.Mutex
	onCodecStateChange []func(webrtc.RTPCodecParameters, ReceiverCodecState)

	streamTrackerManagerListener StreamTrackerManagerListener

	isRED bool
	// RAJA-TODO onCloseHandler     func()
	// RAJA-TODO closeOnce          sync.Once
	videoLayerMode livekit.VideoLayer_Mode

	bufferMu  sync.RWMutex
	buffers   [buffer.DefaultMaxLayerSpatial + 1]buffer.BufferProvider
	trackInfo *livekit.TrackInfo

	videoSizeMu        sync.RWMutex
	videoSizes         [buffer.DefaultMaxLayerSpatial + 1]buffer.VideoSize
	onVideoSizeChanged func()

	rtt uint32

	streamTrackerManager *StreamTrackerManager

	downTrackSpreader *sfuutils.DownTrackSpreader[TrackSender]

	onMaxLayerChange func(mimeType mime.MimeType, maxLayer int32)

	redTransformer atomic.Value // redTransformer interface

	isClosed atomic.Bool
}

func NewReceiverBase(
	trackID livekit.TrackID,
	streamID string,
	kind webrtc.RTPCodecType,
	codec webrtc.RTPCodecParameters,
	headerExtensions []webrtc.RTPHeaderExtensionParameter,
	trackInfo *livekit.TrackInfo,
	logger logger.Logger,
	streamTrackerManagerConfig StreamTrackerManagerConfig,
	streamTrackerManagerListener StreamTrackerManagerListener,
) *ReceiverBase {
	r := &ReceiverBase{
		logger:                       logger,
		trackID:                      trackID,
		streamID:                     streamID,
		kind:                         kind,
		codec:                        codec,
		headerExtensions:             headerExtensions,
		codecState:                   ReceiverCodecStateNormal,
		streamTrackerManagerListener: streamTrackerManagerListener,
		isRED:                        mime.IsMimeTypeStringRED(codec.MimeType),
		trackInfo:                    utils.CloneProto(trackInfo),
		videoLayerMode:               buffer.GetVideoLayerModeForMimeType(mime.NormalizeMimeType(codec.MimeType), trackInfo),
	}

	r.downTrackSpreader = sfuutils.NewDownTrackSpreader[TrackSender](sfuutils.DownTrackSpreaderParams{
		Threshold: r.lbThreshold,
		Logger:    logger,
	})

	r.streamTrackerManager = NewStreamTrackerManager(
		logger,
		trackInfo,
		r.Mime(),
		r.codec.ClockRate,
		streamTrackerManagerConfig,
	)
	r.streamTrackerManager.SetListener(r)

	return r
}

func (r *ReceiverBase) SetPLIThrottleConfig(pliThrottleConfig PLIThrottleConfig) {
	r.pliThrottleConfig = pliThrottleConfig
}

func (r *ReceiverBase) SetAudioConfig(audioConfig AudioConfig) {
	r.audioConfig = audioConfig
}

func (r *ReceiverBase) SetEnableRTPStreamRestartDetection(enableRTPStremRestartDetection bool) {
	r.enableRTPStreamRestartDetection = enableRTPStremRestartDetection
}

func (r *ReceiverBase) SetLBThreshold(lbThreshold int) {
	r.lbThreshold = lbThreshold
}

func (r *ReceiverBase) SetForwardStats(forwardStats *ForwardStats) {
	r.forwardStats = forwardStats
}

func (r *ReceiverBase) TrackInfo() *livekit.TrackInfo {
	r.bufferMu.RLock()
	defer r.bufferMu.RUnlock()

	return utils.CloneProto(r.trackInfo)
}

/* RAJA-TODO - replaced below
func (r *ReceiverBase) UpdateTrackInfo(ti *livekit.TrackInfo) {
	r.trackInfo.Store(utils.CloneProto(ti))
	r.streamTrackerManager.UpdateTrackInfo(ti)
}
*/

func (r *ReceiverBase) UpdateTrackInfo(ti *livekit.TrackInfo) {
	r.bufferMu.Lock()
	existingVersion := utils.TimedVersionFromProto(r.trackInfo.Version)
	updateVersion := utils.TimedVersionFromProto(ti.Version)
	if updateVersion.Compare(existingVersion) < 0 {
		r.bufferMu.Unlock()
		r.logger.Debugw(
			"not updating to older version",
			"existing", logger.Proto(r.trackInfo),
			"updated", logger.Proto(ti),
		)
		return
	}

	shouldResync := utils.TimedVersionFromProto(r.trackInfo.Version) != utils.TimedVersionFromProto(ti.Version)
	if shouldResync {
		r.logger.Debugw(
			"updating track info",
			"existing", logger.Proto(r.trackInfo),
			"updated", logger.Proto(ti),
			"shouldResync", shouldResync,
		)
	}
	r.trackInfo = utils.CloneProto(ti)
	// MUTABLE-TRACKINFO-TODO: notify buffers, buffers may need to resize retransmission buffer if there is layer change

	if shouldResync {
		r.resyncLocked("update-track-info")
	}
	r.bufferMu.Unlock()
}

func (r *ReceiverBase) resyncLocked(reason string) {
	// resync to avoid gaps in the forwarded sequence number
	r.logger.Debugw("resync receiver", "reason", reason)
	r.clearAllBuffersLocked("resync")

	r.downTrackSpreader.Broadcast(func(dt sfu.TrackSender) {
		dt.Resync()
	})
	if rt := r.redTransformer.Load(); rt != nil {
		rt.(sfu.REDTransformer).ResyncDownTracks()
	}
}

func (r *ReceiverBase) OnMaxLayerChange(fn func(mimeType mime.MimeType, maxLayer int32)) {
	r.bufferMu.Lock()
	r.onMaxLayerChange = fn
	r.bufferMu.Unlock()
}

func (r *ReceiverBase) getOnMaxLayerChange() func(mimeType mime.MimeType, maxLayer int32) {
	r.bufferMu.RLock()
	defer r.bufferMu.RUnlock()

	return r.onMaxLayerChange
}

func (r *ReceiverBase) IsClosed() bool {
	return r.isClosed.Load()
}

func (r *ReceiverBase) SetRTT(rtt uint32) {
	r.bufferMu.Lock()
	if r.rtt == rtt {
		r.bufferMu.Unlock()
		return
	}

	if rtt == 0 {
		r.bufferMu.Unlock()
		return
	}

	r.rtt = rtt
	buffers := r.buffers
	r.bufferMu.Unlock()

	for _, buff := range buffers {
		if buff == nil {
			continue
		}

		buff.SetRTT(rtt)
	}
}

func (r *ReceiverBase) TrackID() livekit.TrackID {
	return r.trackID
}

func (r *ReceiverBase) StreamID() string {
	return r.streamID
}

func (r *ReceiverBase) Codec() webrtc.RTPCodecParameters {
	return r.codec
}

func (r *ReceiverBase) Mime() mime.MimeType {
	return mime.NormalizeMimeType(r.codec.MimeType)
}

func (r *ReceiverBase) VideoLayerMode() livekit.VideoLayer_Mode {
	return r.videoLayerMode
}

func (r *ReceiverBase) HeaderExtensions() []webrtc.RTPHeaderExtensionParameter {
	return r.headerExtensions
}

func (r *ReceiverBase) Kind() webrtc.RTPCodecType {
	return r.kind
}

func (r *ReceiverBase) StreamTrackerManager() *StreamTrackerManager {
	return r.streamTrackerManager
}

func (r *ReceiverBase) AddBuffer(buff buffer.BufferProvider, layer int32) {
	buff.SetLogger(r.logger.WithValues("layer", layer))
	buff.SetAudioLevelParams(audio.AudioLevelParams{
		Config: r.audioConfig.AudioLevelConfig,
	})
	buff.SetStreamRestartDetection(r.enableRTPStreamRestartDetection)
	buff.OnRtcpSenderReport(func() {
		srData := buff.GetSenderReportData()
		r.downTrackSpreader.Broadcast(func(dt TrackSender) {
			_ = dt.HandleRTCPSenderReportData(r.codec.PayloadType, layer, srData)
		})

		if rt := r.redTransformer.Load(); rt != nil {
			rt.(REDTransformer).ForwardRTCPSenderReport(r.codec.PayloadType, layer, srData)
		}
	})
	buff.OnVideoSizeChanged(func(videoSize []buffer.VideoSize) {
		r.videoSizeMu.Lock()
		if r.videoLayerMode == livekit.VideoLayer_MULTIPLE_SPATIAL_LAYERS_PER_STREAM {
			copy(r.videoSizes[:], videoSize)
		} else {
			r.videoSizes[layer] = videoSize[0]
		}
		r.logger.Debugw("video size changed", "size", r.videoSizes)
		cb := r.onVideoSizeChanged
		r.videoSizeMu.Unlock()

		if cb != nil {
			cb()
		}
	})
	/* RAJA-TODO
	if r.Kind() == webrtc.RTPCodecTypeVideo && layer == 0 {
		buff.OnCodecChange(w.handleCodecChange)
	}
	*/

	var duration time.Duration
	switch layer {
	case 2:
		duration = r.pliThrottleConfig.HighQuality
	case 1:
		duration = r.pliThrottleConfig.MidQuality
	case 0:
		duration = r.pliThrottleConfig.LowQuality
	default:
		duration = r.pliThrottleConfig.MidQuality
	}
	if duration != 0 {
		buff.SetPLIThrottle(duration.Nanoseconds())
	}

	r.bufferMu.Lock()
	r.buffers[layer] = buff
	rtt := r.rtt
	r.bufferMu.Unlock()

	buff.SetRTT(rtt)
	buff.SetPaused(r.streamTrackerManager.IsPaused())
}

// RAJA-TODO: maybe look up buffer given layer?
func (r *ReceiverBase) StartBuffer(buff buffer.BufferProvider, layer int32) {
	go r.forwardRTP(layer, buff)
	r.logger.Debugw("starting forwarder", "layer", layer)
}

/* RAJA-TODO
func (w *WebRTCReceiver) AddUpTrack(track TrackRemote, buff *buffer.Buffer) error {
	if w.closed.Load() {
		return ErrReceiverClosed
	}

	layer := int32(0)
	if w.Kind() == webrtc.RTPCodecTypeVideo && w.videoLayerMode != livekit.VideoLayer_MULTIPLE_SPATIAL_LAYERS_PER_STREAM {
		layer = buffer.GetSpatialLayerForRid(w.Mime(), track.RID(), w.trackInfo.Load())
	}
	if layer < 0 {
		w.logger.Warnw(
			"invalid layer", nil,
			"rid", track.RID(),
			"trackInfo", logger.Proto(w.trackInfo.Load()),
		)
		return ErrInvalidLayer
	}
	buff.SetLogger(w.logger.WithValues("layer", layer))
	buff.SetAudioLevelParams(audio.AudioLevelParams{
		Config: w.audioConfig.AudioLevelConfig,
	})
	buff.SetAudioLossProxying(w.audioConfig.EnableLossProxying)
	buff.SetStreamRestartDetection(w.enableRTPStreamRestartDetection)
	buff.OnRtcpFeedback(w.sendRTCP)
	buff.OnRtcpSenderReport(func() {
		srData := buff.GetSenderReportData()
		w.downTrackSpreader.Broadcast(func(dt TrackSender) {
			_ = dt.HandleRTCPSenderReportData(w.codec.PayloadType, layer, srData)
		})

		if rt := w.redTransformer.Load(); rt != nil {
			rt.(REDTransformer).ForwardRTCPSenderReport(w.codec.PayloadType, layer, srData)
		}
	})
	buff.OnVideoSizeChanged(func(videoSize []buffer.VideoSize) {
		w.videoSizeMu.Lock()
		if w.videoLayerMode == livekit.VideoLayer_MULTIPLE_SPATIAL_LAYERS_PER_STREAM {
			copy(w.videoSizes[:], videoSize)
		} else {
			w.videoSizes[layer] = videoSize[0]
		}
		w.logger.Debugw("video size changed", "size", w.videoSizes)
		cb := w.onVideoSizeChanged
		w.videoSizeMu.Unlock()

		if cb != nil {
			cb()
		}
	})
	if w.Kind() == webrtc.RTPCodecTypeVideo && layer == 0 {
		buff.OnCodecChange(w.handleCodecChange)
	}

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

	go w.forwardRTP(layer, buff)
	w.logger.Debugw("starting forwarder", "layer", layer)
	return nil
}
*/

// SetUpTrackPaused indicates upstream will not be sending any data.
// this will reflect the "muted" status and will pause streamtracker to ensure we don't turn off
// the layer
func (r *ReceiverBase) SetUpTrackPaused(paused bool) {
	r.streamTrackerManager.SetPaused(paused)

	r.bufferMu.RLock()
	for _, buff := range r.buffers {
		if buff == nil {
			continue
		}

		buff.SetPaused(paused)
	}
	r.bufferMu.RUnlock()
}

func (r *ReceiverBase) AddDownTrack(track TrackSender) error {
	if r.isClosed.Load() {
		return ErrReceiverClosed
	}

	if r.downTrackSpreader.HasDownTrack(track.SubscriberID()) {
		r.logger.Infow("subscriberID already exists, replacing downtrack", "subscriberID", track.SubscriberID())
	}

	track.UpTrackMaxPublishedLayerChange(r.streamTrackerManager.GetMaxPublishedLayer())
	track.UpTrackMaxTemporalLayerSeenChange(r.streamTrackerManager.GetMaxTemporalLayerSeen())

	r.downTrackSpreader.Store(track)
	r.logger.Debugw("downtrack added", "subscriberID", track.SubscriberID())
	return nil
}

func (r *ReceiverBase) DeleteDownTrack(subscriberID livekit.ParticipantID) {
	if r.isClosed.Load() {
		return
	}

	r.downTrackSpreader.Free(subscriberID)
	r.logger.Debugw("downtrack deleted", "subscriberID", subscriberID)
}

func (r *ReceiverBase) GetDownTracks() []TrackSender {
	downTracks := r.downTrackSpreader.GetDownTracks()
	if rt := r.redTransformer.Load(); rt != nil {
		downTracks = append(downTracks, rt.(sfu.REDTransformer).GetDownTracks()...)
	}
	return downTracks
	//return r.downTrackSpreader.GetDownTracks()
}

func (r *ReceiverBase) HasDownTracks() bool {
	if r.downTrackSpreader.DownTrackCount() != 0 {
		return true
	}

	// check if there are downtracks attached to red transformer
	if rt := r.redTransformer.Load(); rt != nil && !rt.(sfu.REDTransformer).CanClose() {
		return true
	}

	return false
}

/* RAJA-TODO - replaced by below?
func (r *ReceiverBase) SetMaxExpectedSpatialLayer(layer int32) {
	r.streamTrackerManager.SetMaxExpectedSpatialLayer(layer)
}
*/

func (r *ReceiverBase) SetMaxExpectedSpatialLayer(layer int32) {
	prevMax := r.streamTrackerManager.SetMaxExpectedSpatialLayer(layer)
	r.logger.Debugw("max expected layer change", "layer", layer, "prevMax", prevMax)

	r.bufferMu.RLock()
	// stop key frame seeders of stopped layers
	for idx := layer + 1; idx <= prevMax; idx++ {
		if r.buffers[idx] != nil {
			r.buffers[idx].StopKeyFrameSeeder()
		}
	}

	// start key frame seeders of newly expected layers
	for idx := prevMax + 1; idx <= layer; idx++ {
		if r.buffers[idx] != nil {
			r.buffers[idx].StartKeyFrameSeeder()
		}
	}
	r.bufferMu.RUnlock()
}

// StreamTrackerManagerListener.OnAvailableLayersChanged
func (r *ReceiverBase) OnAvailableLayersChanged() {
	r.downTrackSpreader.Broadcast(func(dt TrackSender) {
		dt.UpTrackLayersChange()
	})

	if r.streamTrackerManagerListener != nil {
		r.streamTrackerManagerListener.OnAvailableLayersChanged()
	}
}

// StreamTrackerManagerListener.OnBitrateAvailabilityChanged
func (r *ReceiverBase) OnBitrateAvailabilityChanged() {
	r.downTrackSpreader.Broadcast(func(dt TrackSender) {
		dt.UpTrackBitrateAvailabilityChange()
	})

	if r.streamTrackerManagerListener != nil {
		r.streamTrackerManagerListener.OnBitrateAvailabilityChanged()
	}
}

// StreamTrackerManagerListener.OnMaxPublishedLayerChanged
func (r *ReceiverBase) OnMaxPublishedLayerChanged(maxPublishedLayer int32) {
	r.downTrackSpreader.Broadcast(func(dt TrackSender) {
		dt.UpTrackMaxPublishedLayerChange(maxPublishedLayer)
	})

	if r.streamTrackerManagerListener != nil {
		r.streamTrackerManagerListener.OnMaxPublishedLayerChanged(maxPublishedLayer)
	}
}

// StreamTrackerManagerListener.OnMaxTemporalLayerSeenChanged
func (r *ReceiverBase) OnMaxTemporalLayerSeenChanged(maxTemporalLayerSeen int32) {
	r.downTrackSpreader.Broadcast(func(dt TrackSender) {
		dt.UpTrackMaxTemporalLayerSeenChange(maxTemporalLayerSeen)
	})

	if r.streamTrackerManagerListener != nil {
		r.streamTrackerManagerListener.OnMaxTemporalLayerSeenChanged(maxTemporalLayerSeen)
	}
}

// StreamTrackerManagerListener.OnMaxAvailableLayerChanged
func (r *ReceiverBase) OnMaxAvailableLayerChanged(maxAvailableLayer int32) {
	if onMaxLayerChange := r.getOnMaxLayerChange(); onMaxLayerChange != nil {
		onMaxLayerChange(r.Mime(), maxAvailableLayer)
	}

	if r.streamTrackerManagerListener != nil {
		r.streamTrackerManagerListener.OnMaxAvailableLayerChanged(maxAvailableLayer)
	}
}

// StreamTrackerManagerListener.OnBitrateReport
func (r *ReceiverBase) OnBitrateReport(availableLayers []int32, bitrates Bitrates) {
	r.downTrackSpreader.Broadcast(func(dt TrackSender) {
		dt.UpTrackBitrateReport(availableLayers, bitrates)
	})

	if r.streamTrackerManagerListener != nil {
		r.streamTrackerManagerListener.OnBitrateReport(availableLayers, bitrates)
	}
}

func (r *ReceiverBase) GetLayeredBitrate() ([]int32, Bitrates) {
	return r.streamTrackerManager.GetLayeredBitrate()
}

/* RAJA-TODO
// OnCloseHandler method to be called on remote track removed
func (w *WebRTCReceiver) OnCloseHandler(fn func()) {
	w.onCloseHandler = fn
}
*/

func (r *ReceiverBase) SendPLI(layer int32, force bool) {
	// SVC-TODO :  should send LRR (Layer Refresh Request) instead of PLI
	buff := r.getBuffer(layer)
	if buff == nil {
		return
	}

	buff.SendPLI(force)
}

func (r *ReceiverBase) getBuffer(layer int32) buffer.BufferProvider {
	r.bufferMu.RLock()
	defer r.bufferMu.RUnlock()

	return r.getBufferLocked(layer)
}

func (r *ReceiverBase) getBufferLocked(layer int32) buffer.BufferProvider {
	// for svc codecs, use layer = 0 always.
	// spatial layers are in-built and handled by single buffer
	if r.videoLayerMode == livekit.VideoLayer_MULTIPLE_SPATIAL_LAYERS_PER_STREAM {
		layer = 0
	}

	if layer < 0 || int(layer) >= len(r.buffers) {
		return nil
	}

	return r.buffers[layer]
}

func (r *ReceiverBase) GetAllBuffers() [buffer.DefaultMaxLayerSpatial + 1]buffer.BufferProvider {
	buffers := [buffer.DefaultMaxLayerSpatial + 1]buffer.BufferProvider{}

	r.bufferMu.RLock()
	defer r.bufferMu.RUnlock()

	for i := range buffers {
		buffers[i] = r.buffers[i]
	}
	return buffers
}

func (r *ReceiverBase) ClearAllBuffers(reason string) {
	r.bufferMu.Lock()
	defer r.bufferMu.Unlock()

	r.clearAllBuffersLocked(reason)
}

func (r *ReceiverBase) clearAllBuffersLocked(reason string) {
	for idx := 0; idx < len(r.buffers); idx++ {
		if r.buffers[idx] != nil {
			r.buffers[idx].CloseWithReason(reason)
		}
		r.buffers[idx] = nil
	}

	r.streamTrackerManager.RemoveAllTrackers()
}

func (r *ReceiverBase) ReadRTP(buf []byte, layer uint8, esn uint64) (int, error) {
	b := r.getBuffer(int32(layer))
	if b == nil {
		return 0, bucket.ErrPacketMismatch
	}

	return b.GetPacket(buf, esn)
}

func (r *ReceiverBase) GetTrackStats() *livekit.RTPStats {
	r.bufferMu.RLock()
	defer r.bufferMu.RUnlock()

	allStats := make([]*livekit.RTPStats, 0, len(r.buffers))
	for _, buff := range r.buffers {
		if buff == nil {
			continue
		}

		stats := buff.GetStats()
		if stats == nil {
			continue
		}

		allStats = append(allStats, stats)
	}

	return rtpstats.AggregateRTPStats(allStats)
}

func (r *ReceiverBase) GetAudioLevel() (float64, bool) {
	if r.Kind() == webrtc.RTPCodecTypeVideo {
		return 0, false
	}

	r.bufferMu.RLock()
	defer r.bufferMu.RUnlock()

	for _, buff := range r.buffers {
		if buff == nil {
			continue
		}

		return buff.GetAudioLevel()
	}

	return 0, false
}

func (r *ReceiverBase) forwardRTP(layer int32, buff buffer.BufferProvider) {
	// RAJA-TODO: relay Buffer does rb.ReadLoopDone()

	numPacketsForwarded := 0
	numPacketsDropped := 0
	defer func() {
		/* RAJA-TODO
		r.closeOnce.Do(func() {
			r.isClosed.Store(true)
			r.closeTracks()
			if rt := r.redTransformer.Load(); rt != nil {
				rt.(REDTransformer).Close()
			}
		})
		*/

		r.streamTrackerManager.RemoveTracker(layer)
		if r.videoLayerMode == livekit.VideoLayer_MULTIPLE_SPATIAL_LAYERS_PER_STREAM {
			r.streamTrackerManager.RemoveAllTrackers()
		}

		r.logger.Debugw(
			"closing forwarder",
			"layer", layer,
			"numPacketsForwarded", numPacketsForwarded,
			"numPacketsDropped", numPacketsDropped,
		)
	}()

	var spatialTrackers [buffer.DefaultMaxLayerSpatial + 1]streamtracker.StreamTrackerWorker
	if layer < 0 || int(layer) >= len(spatialTrackers) {
		r.logger.Errorw("invalid layer", nil, "layer", layer)
		return
	}

	pktBuf := make([]byte, bucket.RTPMaxPktSize)
	r.logger.Debugw("starting forwarding", "layer", layer)
	for {
		pkt, err := buff.ReadExtended(pktBuf)
		if err == io.EOF {
			return
		}
		dequeuedAt := mono.UnixNano()

		if pkt.IsRestart {
			r.logger.Infow("stream restarted", "layer", layer)
			r.downTrackSpreader.Broadcast(func(dt TrackSender) {
				dt.ReceiverRestart()
			})

			if rt := r.redTransformer.Load(); rt != nil {
				rt.(REDTransformer).OnStreamRestart()
			}
		}

		if pkt.Packet.PayloadType != uint8(r.codec.PayloadType) {
			// drop packets as we don't support codec fallback directly
			r.logger.Debugw(
				"dropping packet - payload mismatch",
				"packetPayloadType", pkt.Packet.PayloadType,
				"payloadType", r.codec.PayloadType,
			)
			numPacketsDropped++
			continue
		}

		spatialLayer := layer
		if pkt.Spatial >= 0 {
			// svc packet, take spatial layer info from packet
			spatialLayer = pkt.Spatial
		}
		if int(spatialLayer) >= len(spatialTrackers) {
			r.logger.Errorw(
				"unexpected spatial layer", nil,
				"spatialLayer", spatialLayer,
				"pktSpatialLayer", pkt.Spatial,
			)
			numPacketsDropped++
			continue
		}

		var writeCount atomic.Int32
		r.downTrackSpreader.Broadcast(func(dt TrackSender) {
			writeCount.Add(dt.WriteRTP(pkt, spatialLayer))
		})

		if rt := r.redTransformer.Load(); rt != nil {
			writeCount.Add(rt.(REDTransformer).ForwardRTP(pkt, spatialLayer))
		}

		// track delay/jitter
		if writeCount.Load() > 0 && r.forwardStats != nil && !pkt.IsBuffered {
			if latency, isHigh := r.forwardStats.Update(pkt.Arrival, mono.UnixNano()); isHigh {
				r.logger.Debugw(
					"high forwarding latency",
					"latency", time.Duration(latency),
					"queuingLatency", time.Duration(dequeuedAt-pkt.Arrival),
					"writeCount", writeCount.Load(),
					"isOutOfOrder", pkt.IsOutOfOrder,
					"layer", layer,
				)
			}
		}

		// track video layers
		if r.Kind() == webrtc.RTPCodecTypeVideo {
			if spatialTrackers[spatialLayer] == nil {
				spatialTrackers[spatialLayer] = r.streamTrackerManager.GetTracker(spatialLayer)
				if spatialTrackers[spatialLayer] == nil {
					if r.videoLayerMode == livekit.VideoLayer_MULTIPLE_SPATIAL_LAYERS_PER_STREAM && pkt.DependencyDescriptor != nil {
						r.streamTrackerManager.AddDependencyDescriptorTrackers()
					}
					spatialTrackers[spatialLayer] = r.streamTrackerManager.AddTracker(spatialLayer)
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

		numPacketsForwarded++

		buffer.ReleaseExtPacket(pkt)
	}
}

// RAJA-TODO - this is not in relay receiver, can this be re-used
// closeTracks close all tracks from Receiver
func (r *ReceiverBase) closeTracks() {
	// RAJA-TODO w.connectionStats.Close()
	r.streamTrackerManager.Close()

	// RAJA-TODO closeTrackSenders(r.downTrackSpreader.ResetAndGetDownTracks())

	/* RAJA-TODO
	if w.onCloseHandler != nil {
		w.onCloseHandler()
	}
	*/
}

func (r *ReceiverBase) DebugInfo() map[string]any {
	var videoLayerMode livekit.VideoLayer_Mode
	if ti := r.trackInfo.Load(); ti != nil {
		videoLayerMode = buffer.GetVideoLayerModeForMimeType(r.Mime(), ti)
	}
	info := map[string]any{
		"Mime":           r.Mime().String(),
		"VideoLayerMode": videoLayerMode.String(),
	}

	return info
}

func (r *ReceiverBase) GetPrimaryReceiverForRed() TrackReceiver {
	r.bufferMu.Lock()
	defer r.bufferMu.Unlock()

	if !r.isRED || r.isClosed.Load() {
		return r
	}

	rt := r.redTransformer.Load()
	if rt == nil {
		pr := NewRedPrimaryReceiver(r, sfuutils.DownTrackSpreaderParams{
			Threshold: r.lbThreshold,
			Logger:    r.logger,
		})
		r.redTransformer.Store(pr)
		return pr
	} else {
		if pr, ok := rt.(*RedPrimaryReceiver); ok {
			return pr
		}
	}
	return nil
}

func (r *ReceiverBase) GetRedReceiver() TrackReceiver {
	r.bufferMu.Lock()
	defer r.bufferMu.Unlock()

	if r.isRED || r.isClosed.Load() {
		return r
	}

	rt := r.redTransformer.Load()
	if rt == nil {
		pr := NewRedReceiver(r, sfuutils.DownTrackSpreaderParams{
			Threshold: r.lbThreshold,
			Logger:    r.logger,
		})
		r.redTransformer.Store(pr)
		return pr
	} else {
		if pr, ok := rt.(*RedReceiver); ok {
			return pr
		}
	}
	return nil
}

func (r *ReceiverBase) GetTemporalLayerFpsForSpatial(layer int32) []float32 {
	b := r.getBuffer(layer)
	if b == nil {
		return nil
	}

	if r.videoLayerMode != livekit.VideoLayer_MULTIPLE_SPATIAL_LAYERS_PER_STREAM {
		return b.GetTemporalLayerFpsForSpatial(0)
	}

	return b.GetTemporalLayerFpsForSpatial(layer)
}

func (r *ReceiverBase) AddOnReady(fn func()) {
	// receiver is always ready after created
	fn()
}

/* RAJA-TODO
func (w *WebRTCReceiver) handleCodecChange(newCodec webrtc.RTPCodecParameters) {
	// we don't support the codec fallback directly, set the codec state to invalid once it happens
	w.SetCodecState(ReceiverCodecStateInvalid)
}
*/

// RAJA-TODO: relayreceiver has this in params, that should pass it on to base
func (r *ReceiverBase) AddOnCodecStateChange(f func(webrtc.RTPCodecParameters, ReceiverCodecState)) {
	r.codecStateLock.Lock()
	r.onCodecStateChange = append(r.onCodecStateChange, f)
	r.codecStateLock.Unlock()
}

func (r *ReceiverBase) CodecState() ReceiverCodecState {
	r.codecStateLock.Lock()
	defer r.codecStateLock.Unlock()

	return r.codecState
}

func (r *ReceiverBase) SetCodecState(state ReceiverCodecState) {
	r.codecStateLock.Lock()
	if r.codecState == state || r.codecState == ReceiverCodecStateInvalid {
		r.codecStateLock.Unlock()
		return
	}

	r.codecState = state
	fns := r.onCodecStateChange
	r.codecStateLock.Unlock()

	for _, f := range fns {
		f(r.codec, state)
	}
}

func (r *ReceiverBase) VideoSizes() []buffer.VideoSize {
	var sizes []buffer.VideoSize
	r.videoSizeMu.RLock()
	defer r.videoSizeMu.RUnlock()
	for _, v := range r.videoSizes {
		if v.Width == 0 || v.Height == 0 {
			break
		}
		sizes = append(sizes, v)
	}

	return sizes
}

// RAJA-TODO: relay receiver is using params - make this works with that
func (r *ReceiverBase) OnVideoSizeChanged(f func()) {
	r.videoSizeMu.Lock()
	r.onVideoSizeChanged = f
	r.videoSizeMu.Unlock()
}

// -----------------------------------------------------------

/* RAJA-TODO
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
*/
