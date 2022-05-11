package rtc

import (
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pion/rtcp"
	"go.uber.org/atomic"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/sfu"
	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/telemetry"
)

const (
	downLostUpdateDelta     = time.Second
	layerSelectionTolerance = 0.9
)

type simulcastReceiver struct {
	sfu.TrackReceiver
	priority int
}

func (r *simulcastReceiver) Priority() int {
	return r.priority
}

type MediaTrackReceiver struct {
	params      MediaTrackReceiverParams
	muted       atomic.Bool
	simulcasted atomic.Bool

	lock      sync.RWMutex
	receivers []*simulcastReceiver // MimeType -> TrackReceiver
	// receiver  sfu.TrackReceiver
	// altReceiver     sfu.TrackReceiver
	layerDimensions sync.Map // livekit.VideoQuality => *livekit.VideoLayer

	// track audio fraction lost
	downFracLostLock   sync.Mutex
	maxDownFracLost    uint8
	maxDownFracLostTs  time.Time
	onMediaLossUpdate  func(fractionalLoss uint8)
	onVideoLayerUpdate func(layers []*livekit.VideoLayer)

	onClose []func()

	*MediaTrackSubscriptions
}

type MediaTrackReceiverParams struct {
	TrackInfo           *livekit.TrackInfo
	MediaTrack          types.MediaTrack
	ParticipantID       livekit.ParticipantID
	ParticipantIdentity livekit.ParticipantIdentity
	BufferFactory       *buffer.Factory
	ReceiverConfig      ReceiverConfig
	SubscriberConfig    DirectionConfig
	VideoConfig         config.VideoConfig
	Telemetry           telemetry.TelemetryService
	Logger              logger.Logger
}

func NewMediaTrackReceiver(params MediaTrackReceiverParams) *MediaTrackReceiver {
	t := &MediaTrackReceiver{
		params: params,
	}

	t.MediaTrackSubscriptions = NewMediaTrackSubscriptions(MediaTrackSubscriptionsParams{
		MediaTrack:       params.MediaTrack,
		BufferFactory:    params.BufferFactory,
		ReceiverConfig:   params.ReceiverConfig,
		SubscriberConfig: params.SubscriberConfig,
		VideoConfig:      t.params.VideoConfig,
		Telemetry:        params.Telemetry,
		Logger:           params.Logger,
	})

	if params.TrackInfo.Muted {
		t.SetMuted(true)
	}

	if params.TrackInfo != nil && t.Kind() == livekit.TrackType_VIDEO {
		t.UpdateVideoLayers(params.TrackInfo.Layers)
		// LK-TODO: maybe use this or simulcast flag in TrackInfo to set simulcasted here
	}

	return t
}

func (t *MediaTrackReceiver) Restart() {
	t.lock.Lock()
	receivers := t.receivers
	t.lock.Unlock()

	for _, receiver := range receivers {
		receiver.SetMaxExpectedSpatialLayer(SpatialLayerForQuality(livekit.VideoQuality_HIGH))
	}

	t.MediaTrackSubscriptions.Restart()
}

func (t *MediaTrackReceiver) SetupReceiver(receiver sfu.TrackReceiver, priority int) {
	t.lock.Lock()
	t.receivers = append(t.receivers, &simulcastReceiver{receiver, priority})
	sort.Slice(t.receivers, func(i, j int) bool {
		return t.receivers[i].Priority() < t.receivers[j].Priority()
	})
	t.lock.Unlock()
	t.params.Logger.Debugw("setup receiver", "mime", receiver.Codec().MimeType, "priority", priority, "receivers", t.receivers)
	t.MediaTrackSubscriptions.AddCodec(receiver.Codec().MimeType)

	t.MediaTrackSubscriptions.Start()
}

func (t *MediaTrackReceiver) ClearReceiver(mime string) {
	t.lock.Lock()
	for idx, receiver := range t.receivers {
		if strings.EqualFold(receiver.Codec().MimeType, mime) {
			t.receivers[idx] = t.receivers[len(t.receivers)-1]
			t.receivers = t.receivers[:len(t.receivers)-1]
			break
		}
	}
	closeSubscription := len(t.receivers) == 0
	t.lock.Unlock()

	if closeSubscription {
		t.MediaTrackSubscriptions.Close()
	}
}

func (t *MediaTrackReceiver) ClearAllReceivers() {
	t.lock.Lock()
	t.receivers = t.receivers[:0]
	t.lock.Unlock()
	t.MediaTrackSubscriptions.Close()
}

func (t *MediaTrackReceiver) OnMediaLossUpdate(f func(fractionalLoss uint8)) {
	t.onMediaLossUpdate = f
}

func (t *MediaTrackReceiver) OnVideoLayerUpdate(f func(layers []*livekit.VideoLayer)) {
	t.onVideoLayerUpdate = f
}

func (t *MediaTrackReceiver) TryClose() bool {
	t.lock.Lock()
	if len(t.receivers) > 0 {
		t.lock.Unlock()
		return false
	}
	onclose := t.onClose
	t.lock.Unlock()

	t.MediaTrackSubscriptions.Close()

	for _, f := range onclose {
		f()
	}
	return true
}

func (t *MediaTrackReceiver) ID() livekit.TrackID {
	return livekit.TrackID(t.params.TrackInfo.Sid)
}

func (t *MediaTrackReceiver) Kind() livekit.TrackType {
	return t.params.TrackInfo.Type
}

func (t *MediaTrackReceiver) Source() livekit.TrackSource {
	return t.params.TrackInfo.Source
}

func (t *MediaTrackReceiver) PublisherID() livekit.ParticipantID {
	return t.params.ParticipantID
}

func (t *MediaTrackReceiver) PublisherIdentity() livekit.ParticipantIdentity {
	return t.params.ParticipantIdentity
}

func (t *MediaTrackReceiver) IsSimulcast() bool {
	return t.simulcasted.Load()
}

func (t *MediaTrackReceiver) SetSimulcast(simulcast bool) {
	t.simulcasted.Store(simulcast)
}

func (t *MediaTrackReceiver) Name() string {
	return t.params.TrackInfo.Name
}

func (t *MediaTrackReceiver) IsMuted() bool {
	return t.muted.Load()
}

func (t *MediaTrackReceiver) SetMuted(muted bool) {
	t.muted.Store(muted)

	t.lock.RLock()
	for _, receiver := range t.receivers {
		receiver.SetUpTrackPaused(muted)
	}
	t.lock.RUnlock()

	t.MediaTrackSubscriptions.SetMuted(muted)
}

func (t *MediaTrackReceiver) AddOnClose(f func()) {
	if f == nil {
		return
	}

	t.lock.Lock()
	t.onClose = append(t.onClose, f)
	t.lock.Unlock()
}

// AddSubscriber subscribes sub to current mediaTrack
func (t *MediaTrackReceiver) AddSubscriber(sub types.LocalParticipant) error {
	t.lock.RLock()
	receivers := t.receivers
	t.lock.RUnlock()

	if len(receivers) == 0 {
		// cannot add, no receiver
		return errors.New("cannot subscribe without a receiver in place")
	}

	// using DownTrack from ion-sfu
	streamId := string(t.PublisherID())
	if sub.ProtocolVersion().SupportsPackedStreamId() {
		// when possible, pack both IDs in streamID to allow new streams to be generated
		// react-native-webrtc still uses stream based APIs and require this
		streamId = PackStreamID(t.PublisherID(), t.ID())
	}

	downTrack, err := t.MediaTrackSubscriptions.AddSubscriber(sub, NewWrappedReceiver(receivers, t.ID(), streamId))
	if err != nil {
		return err
	}

	if downTrack != nil {
		if t.Kind() == livekit.TrackType_AUDIO {
			downTrack.AddReceiverReportListener(t.handleMaxLossFeedback)
		}

	}
	return nil
}

func (t *MediaTrackReceiver) UpdateTrackInfo(ti *livekit.TrackInfo) {
	t.params.TrackInfo = ti
	if ti != nil && t.Kind() == livekit.TrackType_VIDEO {
		t.UpdateVideoLayers(ti.Layers)
	}
}

func (t *MediaTrackReceiver) TrackInfo() *livekit.TrackInfo {
	return t.params.TrackInfo
}

func (t *MediaTrackReceiver) UpdateVideoLayers(layers []*livekit.VideoLayer) {
	for _, layer := range layers {
		t.layerDimensions.Store(layer.Quality, layer)
	}

	t.MediaTrackSubscriptions.UpdateVideoLayers()
	if t.onVideoLayerUpdate != nil {
		t.onVideoLayerUpdate(layers)
	}

	// TODO: this might need to trigger a participant update for clients to pick up dimension change
}

func (t *MediaTrackReceiver) GetVideoLayers() []*livekit.VideoLayer {
	layers := make([]*livekit.VideoLayer, 0)
	t.layerDimensions.Range(func(q, val interface{}) bool {
		if layer, ok := val.(*livekit.VideoLayer); ok {
			layers = append(layers, layer)
		}
		return true
	})

	return layers
}

// GetQualityForDimension finds the closest quality to use for desired dimensions
// affords a 20% tolerance on dimension
func (t *MediaTrackReceiver) GetQualityForDimension(width, height uint32) livekit.VideoQuality {
	quality := livekit.VideoQuality_HIGH
	if t.Kind() == livekit.TrackType_AUDIO || t.params.TrackInfo.Height == 0 {
		return quality
	}
	origSize := t.params.TrackInfo.Height
	requestedSize := height
	if t.params.TrackInfo.Width < t.params.TrackInfo.Height {
		// for portrait videos
		origSize = t.params.TrackInfo.Width
		requestedSize = width
	}

	// default sizes representing qualities low - high
	layerSizes := []uint32{180, 360, origSize}
	var providedSizes []uint32
	t.layerDimensions.Range(func(_, val interface{}) bool {
		if layer, ok := val.(*livekit.VideoLayer); ok {
			providedSizes = append(providedSizes, layer.Height)
		}
		return true
	})
	if len(providedSizes) > 0 {
		layerSizes = providedSizes
		// comparing height always
		requestedSize = height
		sort.Slice(layerSizes, func(i, j int) bool {
			return layerSizes[i] < layerSizes[j]
		})
	}

	// finds the lowest layer that could satisfy client demands
	requestedSize = uint32(float32(requestedSize) * layerSelectionTolerance)
	for i, s := range layerSizes {
		quality = livekit.VideoQuality(i)
		if s >= requestedSize {
			break
		}
	}

	return quality
}

func (t *MediaTrackReceiver) GetAudioLevel() (float64, bool) {
	receiver := t.PrimaryReceiver()
	if receiver == nil {
		return 0, false
	}

	return receiver.GetAudioLevel()
}

// handles max loss for audio streams
func (t *MediaTrackReceiver) handleMaxLossFeedback(_ *sfu.DownTrack, report *rtcp.ReceiverReport) {
	t.downFracLostLock.Lock()
	for _, rr := range report.Reports {
		if t.maxDownFracLost < rr.FractionLost {
			t.maxDownFracLost = rr.FractionLost
		}
	}
	t.downFracLostLock.Unlock()

	t.maybeUpdateLoss()
}

func (t *MediaTrackReceiver) NotifySubscriberNodeMediaLoss(_nodeID livekit.NodeID, fractionalLoss uint8) {
	t.downFracLostLock.Lock()
	if t.maxDownFracLost < fractionalLoss {
		t.maxDownFracLost = fractionalLoss
	}
	t.downFracLostLock.Unlock()

	t.maybeUpdateLoss()
}

func (t *MediaTrackReceiver) maybeUpdateLoss() {
	var (
		shouldUpdate bool
		maxLost      uint8
	)

	t.downFracLostLock.Lock()
	now := time.Now()
	if now.Sub(t.maxDownFracLostTs) > downLostUpdateDelta {
		shouldUpdate = true
		maxLost = t.maxDownFracLost
		t.maxDownFracLost = 0
		t.maxDownFracLostTs = now
	}
	t.downFracLostLock.Unlock()

	if shouldUpdate {
		if t.onMediaLossUpdate != nil {
			t.onMediaLossUpdate(maxLost)
		}
	}
}

func (t *MediaTrackReceiver) DebugInfo() map[string]interface{} {
	info := map[string]interface{}{
		"ID":       t.ID(),
		"Kind":     t.Kind().String(),
		"PubMuted": t.muted.Load(),
	}

	info["DownTracks"] = t.MediaTrackSubscriptions.DebugInfo()

	t.lock.RLock()
	for _, receiver := range t.receivers {
		info[receiver.Codec().MimeType] = receiver.DebugInfo()
	}
	t.lock.RUnlock()
	return info
}

func (t *MediaTrackReceiver) PrimaryReceiver() sfu.TrackReceiver {
	t.lock.RLock()
	defer t.lock.RUnlock()

	if len(t.receivers) == 0 {
		return nil
	}
	return t.receivers[0].TrackReceiver
}

func (t *MediaTrackReceiver) Receiver(mime string) sfu.TrackReceiver {
	t.lock.RLock()
	defer t.lock.RUnlock()

	for _, r := range t.receivers {
		if strings.EqualFold(r.Codec().MimeType, mime) {
			return r.TrackReceiver
		}
	}
	return nil
}

func (t *MediaTrackReceiver) SetRTT(rtt uint32) {
	t.lock.Lock()
	defer t.lock.Unlock()

	for _, r := range t.receivers {
		r.TrackReceiver.(*sfu.WebRTCReceiver).SetRTT(rtt)
	}
}

func (t *MediaTrackReceiver) OnSubscribedMaxQualityChange(f func(trackID livekit.TrackID, subscribedQualities []*livekit.SubscribedCodec, maxSubscribedQualities []types.SubscribedCodecQuality) error) {
	t.MediaTrackSubscriptions.OnSubscribedMaxQualityChange(func(subscribedQualities []*livekit.SubscribedCodec, maxSubscribedQualities []types.SubscribedCodecQuality) {
		if f != nil && !t.IsMuted() {
			_ = f(t.ID(), subscribedQualities, maxSubscribedQualities)
		}
		for _, q := range maxSubscribedQualities {
			receiver := t.Receiver(q.CodecMime)
			if receiver != nil {
				receiver.SetMaxExpectedSpatialLayer(SpatialLayerForQuality(q.Quality))
			}
		}
	})
}

// ---------------------------

func SpatialLayerForQuality(quality livekit.VideoQuality) int32 {
	switch quality {
	case livekit.VideoQuality_LOW:
		return 0
	case livekit.VideoQuality_MEDIUM:
		return 1
	case livekit.VideoQuality_HIGH:
		return 2
	case livekit.VideoQuality_OFF:
		return -1
	default:
		return -1
	}
}

func QualityForSpatialLayer(layer int32) livekit.VideoQuality {
	switch layer {
	case 0:
		return livekit.VideoQuality_LOW
	case 1:
		return livekit.VideoQuality_MEDIUM
	case 2:
		return livekit.VideoQuality_HIGH
	case sfu.InvalidLayerSpatial:
		return livekit.VideoQuality_OFF
	default:
		return livekit.VideoQuality_OFF
	}
}

func VideoQualityToRID(q livekit.VideoQuality) string {
	switch q {
	case livekit.VideoQuality_HIGH:
		return sfu.FullResolution
	case livekit.VideoQuality_MEDIUM:
		return sfu.HalfResolution
	default:
		return sfu.QuarterResolution
	}
}
