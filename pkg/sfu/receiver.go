package sfu

import (
	"errors"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"
	"go.uber.org/atomic"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/sfu/audio"
	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/sfu/connectionquality"
	"github.com/livekit/livekit-server/pkg/sfu/twcc"
)

var (
	ErrReceiverClosed        = errors.New("receiver closed")
	ErrDownTrackAlreadyExist = errors.New("DownTrack already exist")
)

type AudioLevelHandle func(level uint8, duration uint32)
type Bitrates [DefaultMaxLayerSpatial + 1][DefaultMaxLayerTemporal + 1]int64

// TrackReceiver defines an interface receive media from remote peer
type TrackReceiver interface {
	TrackID() livekit.TrackID
	StreamID() string
	Codec() webrtc.RTPCodecParameters
	HeaderExtensions() []webrtc.RTPHeaderExtensionParameter

	ReadRTP(buf []byte, layer uint8, sn uint16) (int, error)
	GetBitrateTemporalCumulative() Bitrates

	GetAudioLevel() (float64, bool)

	SendPLI(layer int32, force bool)

	SetUpTrackPaused(paused bool)
	SetMaxExpectedSpatialLayer(layer int32)

	AddDownTrack(track TrackSender) error
	DeleteDownTrack(participantID livekit.ParticipantID)

	DebugInfo() map[string]interface{}

	GetLayerDimension(layer int32) (uint32, uint32)
	TrackInfo() *livekit.TrackInfo

	// Get primary receiver if this receiver represents a RED codec; otherwise it will return itself
	GetPrimaryReceiverForRed() TrackReceiver
}

// WebRTCReceiver receives a media track
type WebRTCReceiver struct {
	logger logger.Logger

	pliThrottleConfig config.PLIThrottleConfig
	audioConfig       config.AudioConfig

	trackID        livekit.TrackID
	streamID       string
	kind           webrtc.RTPCodecType
	receiver       *webrtc.RTPReceiver
	codec          webrtc.RTPCodecParameters
	isSimulcast    bool
	isSVC          bool
	isRED          bool
	onCloseHandler func()
	closeOnce      sync.Once
	closed         atomic.Bool
	useTrackers    bool
	trackInfo      *livekit.TrackInfo

	rtcpCh chan []rtcp.Packet

	twcc *twcc.Responder

	bufferMu sync.RWMutex
	buffers  [DefaultMaxLayerSpatial + 1]*buffer.Buffer
	rtt      uint32

	upTrackMu sync.RWMutex
	upTracks  [DefaultMaxLayerSpatial + 1]*webrtc.TrackRemote

	lbThreshold int

	streamTrackerManager *StreamTrackerManager

	downTrackSpreader *DownTrackSpreader

	connectionStats *connectionquality.ConnectionStats

	// update stats
	onStatsUpdate func(w *WebRTCReceiver, stat *livekit.AnalyticsStat)

	// update layer info
	onMaxLayerChange func(maxLayer int32)

	primaryReceiver atomic.Value // *RedPrimaryReceiver
}

func IsSvcCodec(mime string) bool {
	switch strings.ToLower(mime) {
	case "video/av1":
		fallthrough
	case "video/vp9":
		return true
	}
	return false
}

func IsRedCodec(mime string) bool {
	return strings.HasSuffix(strings.ToLower(mime), "red")
}

type ReceiverOpts func(w *WebRTCReceiver) *WebRTCReceiver

// WithPliThrottleConfig indicates minimum time(ms) between sending PLIs
func WithPliThrottleConfig(pliThrottleConfig config.PLIThrottleConfig) ReceiverOpts {
	return func(w *WebRTCReceiver) *WebRTCReceiver {
		w.pliThrottleConfig = pliThrottleConfig
		return w
	}
}

// WithAudioConfig sets up parameters for active speaker detection
func WithAudioConfig(audioConfig config.AudioConfig) ReceiverOpts {
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

// NewWebRTCReceiver creates a new webrtc track receiver
func NewWebRTCReceiver(
	receiver *webrtc.RTPReceiver,
	track *webrtc.TrackRemote,
	trackInfo *livekit.TrackInfo,
	logger logger.Logger,
	twcc *twcc.Responder,
	opts ...ReceiverOpts,
) *WebRTCReceiver {
	w := &WebRTCReceiver{
		logger:   logger,
		receiver: receiver,
		trackID:  livekit.TrackID(track.ID()),
		streamID: track.StreamID(),
		codec:    track.Codec(),
		kind:     track.Kind(),
		// LK-TODO: this should be based on VideoLayers protocol message rather than RID based
		isSimulcast:          len(track.RID()) > 0,
		twcc:                 twcc,
		streamTrackerManager: NewStreamTrackerManager(logger, trackInfo),
		trackInfo:            trackInfo,
		isSVC:                IsSvcCodec(track.Codec().MimeType),
		isRED:                IsRedCodec(track.Codec().MimeType),
	}

	w.streamTrackerManager.OnMaxLayerChanged(w.onMaxLayerChange)
	w.streamTrackerManager.OnAvailableLayersChanged(w.downTrackLayerChange)
	w.streamTrackerManager.OnBitrateAvailabilityChanged(w.downTrackBitrateAvailabilityChange)

	for _, opt := range opts {
		w = opt(w)
	}

	w.downTrackSpreader = NewDownTrackSpreader(DownTrackSpreaderParams{
		Threshold: w.lbThreshold,
		Logger:    logger,
	})

	w.connectionStats = connectionquality.NewConnectionStats(connectionquality.ConnectionStatsParams{
		MimeType:            w.codec.MimeType,
		GetDeltaStats:       w.getDeltaStats,
		GetMaxExpectedLayer: w.streamTrackerManager.GetMaxExpectedLayer,
		GetIsReducedQuality: func() (int32, bool) {
			distance := w.streamTrackerManager.DistanceToDesired()
			return distance, distance > 0
		},
		Logger: w.logger,
	})
	w.connectionStats.OnStatsUpdate(func(_cs *connectionquality.ConnectionStats, stat *livekit.AnalyticsStat) {
		if w.onStatsUpdate != nil {
			w.onStatsUpdate(w, stat)
		}
	})
	w.connectionStats.Start(w.trackInfo)

	return w
}

func (w *WebRTCReceiver) TrackInfo() *livekit.TrackInfo {
	return w.trackInfo
}

func (w *WebRTCReceiver) GetLayerDimension(layer int32) (uint32, uint32) {
	return w.streamTrackerManager.GetLayerDimension(layer)
}

func (w *WebRTCReceiver) OnStatsUpdate(fn func(w *WebRTCReceiver, stat *livekit.AnalyticsStat)) {
	w.onStatsUpdate = fn
}

func (w *WebRTCReceiver) OnMaxLayerChange(fn func(maxLayer int32)) {
	w.streamTrackerManager.OnMaxLayerChanged(fn)
}

func (w *WebRTCReceiver) GetConnectionScore() float32 {
	return w.connectionStats.GetScore()
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

func (w *WebRTCReceiver) SSRC(layer int) uint32 {
	w.upTrackMu.RLock()
	defer w.upTrackMu.RUnlock()

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

func (w *WebRTCReceiver) AddUpTrack(track *webrtc.TrackRemote, buff *buffer.Buffer) {
	if w.closed.Load() {
		return
	}

	layer := int32(0)
	if w.Kind() == webrtc.RTPCodecTypeVideo {
		layer = buffer.RidToSpatialLayer(track.RID(), w.trackInfo)
	}
	buff.SetLogger(logger.Logger(logr.Logger(w.logger).WithValues("layer", layer)))
	buff.SetTWCC(w.twcc)
	buff.SetAudioLevelParams(audio.AudioLevelParams{
		ActiveLevel:     w.audioConfig.ActiveLevel,
		MinPercentile:   w.audioConfig.MinPercentile,
		ObserveDuration: w.audioConfig.UpdateInterval,
		SmoothIntervals: w.audioConfig.SmoothIntervals,
	})
	buff.OnRtcpFeedback(w.sendRTCP)

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

	w.upTrackMu.Lock()
	w.upTracks[layer] = track
	w.upTrackMu.Unlock()

	w.bufferMu.Lock()
	w.buffers[layer] = buff
	rtt := w.rtt
	w.bufferMu.Unlock()
	buff.SetRTT(rtt)

	if w.Kind() == webrtc.RTPCodecTypeVideo && w.useTrackers {
		w.streamTrackerManager.AddTracker(layer)
	}

	go w.forwardRTP(layer)
}

// SetUpTrackPaused indicates upstream will not be sending any data.
// this will reflect the "muted" status and will pause streamtracker to ensure we don't turn off
// the layer
func (w *WebRTCReceiver) SetUpTrackPaused(paused bool) {
	w.streamTrackerManager.SetPaused(paused)
}

func (w *WebRTCReceiver) AddDownTrack(track TrackSender) error {
	if w.closed.Load() {
		return ErrReceiverClosed
	}

	if w.downTrackSpreader.HasDownTrack(track.SubscriberID()) {
		w.logger.Infow("subscriberID already exists, replace the downtrack", "subscriberID", track.SubscriberID())
	}

	if w.Kind() == webrtc.RTPCodecTypeVideo {
		// notify added down track of available layers
		availableLayers, exemptedLayers := w.streamTrackerManager.GetAvailableLayers()
		track.UpTrackLayersChange(availableLayers, exemptedLayers)
	}
	track.TrackInfoAvailable()

	w.downTrackSpreader.Store(track)
	return nil
}

func (w *WebRTCReceiver) SetMaxExpectedSpatialLayer(layer int32) {
	w.streamTrackerManager.SetMaxExpectedSpatialLayer(layer)
}

func (w *WebRTCReceiver) downTrackLayerChange(availableLayers []int32, exemptedLayers []int32) {
	for _, dt := range w.downTrackSpreader.GetDownTracks() {
		dt.UpTrackLayersChange(availableLayers, exemptedLayers)
	}
}

func (w *WebRTCReceiver) downTrackBitrateAvailabilityChange() {
	for _, dt := range w.downTrackSpreader.GetDownTracks() {
		dt.UpTrackBitrateAvailabilityChange()
	}
}

func (w *WebRTCReceiver) GetBitrateTemporalCumulative() Bitrates {
	return w.streamTrackerManager.GetBitrateTemporalCumulative()
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
}

func (w *WebRTCReceiver) sendRTCP(packets []rtcp.Packet) {
	if packets == nil || w.closed.Load() {
		return
	}

	select {
	case w.rtcpCh <- packets:
	default:
		w.logger.Warnw("sendRTCP failed, rtcp channel full", nil)
	}
}

func (w *WebRTCReceiver) SendPLI(layer int32, force bool) {
	// TODO :  should send LRR (Layer Refresh Request) instead of PLI
	buff := w.getBuffer(layer)
	if buff == nil {
		return
	}

	buff.SendPLI(force)
}

func (w *WebRTCReceiver) SetRTCPCh(ch chan []rtcp.Packet) {
	w.rtcpCh = ch
}

func (w *WebRTCReceiver) getBuffer(layer int32) *buffer.Buffer {
	// for svc codecs, use layer full quality instead.
	// we only have buffer for full quality
	if w.isSVC {
		layer = int32(len(w.buffers)) - 1
	}
	w.bufferMu.RLock()
	buff := w.buffers[layer]
	w.bufferMu.RUnlock()
	if buff == nil {
		w.logger.Warnw("getBuffer failed, buffer not found", nil, "layer", layer)
	}
	return buff
}

func (w *WebRTCReceiver) ReadRTP(buf []byte, layer uint8, sn uint16) (int, error) {
	return w.getBuffer(int32(layer)).GetPacket(buf, sn)
}

func (w *WebRTCReceiver) GetTrackStats() *livekit.RTPStats {
	w.bufferMu.RLock()
	defer w.bufferMu.RUnlock()

	var stats []*livekit.RTPStats
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

	return buffer.AggregateRTPStats(stats)
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

func (w *WebRTCReceiver) getDeltaStats() map[uint32]*buffer.StreamStatsWithLayers {
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
		patched := make(map[int32]*buffer.RTPDeltaInfo, 1)
		patched[int32(layer)] = sswl.Layers[0]
		sswl.Layers = patched

		deltaStats[w.SSRC(layer)] = sswl
	}

	return deltaStats
}

func (w *WebRTCReceiver) forwardRTP(layer int32) {
	tracker := w.streamTrackerManager.GetTracker(layer)

	defer func() {
		w.closeOnce.Do(func() {
			w.closed.Store(true)
			w.closeTracks()
			if pr := w.primaryReceiver.Load(); pr != nil {
				pr.(*RedPrimaryReceiver).Close()
			}
		})

		w.streamTrackerManager.RemoveTracker(layer)
		if w.isSVC {
			w.streamTrackerManager.RemoveAllTrackers()
		}
	}()

	for {
		w.bufferMu.RLock()
		buf := w.buffers[layer]
		w.bufferMu.RUnlock()
		pkt, err := buf.ReadExtended()
		if err == io.EOF {
			return
		}

		// svc packet, dispatch to correct tracker
		spatialTracker := tracker
		spatialLayer := layer
		if pkt.Spatial >= 0 {
			spatialLayer = pkt.Spatial
			spatialTracker = w.streamTrackerManager.GetTracker(pkt.Spatial)
			if spatialTracker == nil {
				spatialTracker = w.streamTrackerManager.AddTracker(pkt.Spatial)
			}
		}

		if spatialTracker != nil {
			spatialTracker.Observe(pkt.Packet.SequenceNumber, pkt.Temporal, len(pkt.RawPacket), len(pkt.Packet.Payload))
		}

		w.downTrackSpreader.Broadcast(func(dt TrackSender) {
			_ = dt.WriteRTP(pkt, spatialLayer)
		})
		if pr := w.primaryReceiver.Load(); pr != nil {
			pr.(*RedPrimaryReceiver).ForwardRTP(pkt, spatialLayer)
		}
	}
}

// closeTracks close all tracks from Receiver
func (w *WebRTCReceiver) closeTracks() {
	w.connectionStats.Close()

	for _, dt := range w.downTrackSpreader.ResetAndGetDownTracks() {
		dt.Close()
	}

	if w.onCloseHandler != nil {
		w.onCloseHandler()
	}
}

func (w *WebRTCReceiver) DebugInfo() map[string]interface{} {
	info := map[string]interface{}{
		"Simulcast": w.isSimulcast,
	}

	w.upTrackMu.RLock()
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
	w.upTrackMu.RUnlock()
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
		w.primaryReceiver.CompareAndSwap(nil, pr)
	}
	return w.primaryReceiver.Load().(*RedPrimaryReceiver)
}
