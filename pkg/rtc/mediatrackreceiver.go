package rtc

import (
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/pion/rtcp"
	"go.uber.org/atomic"
	"google.golang.org/protobuf/proto"

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

type MediaTrackReceiver struct {
	params      MediaTrackReceiverParams
	muted       atomic.Bool
	simulcasted atomic.Bool

	lock            sync.RWMutex
	trackInfo       *livekit.TrackInfo
	receiver        sfu.TrackReceiver
	layerDimensions map[livekit.VideoQuality]*livekit.VideoLayer

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
		params:          params,
		trackInfo:       proto.Clone(params.TrackInfo).(*livekit.TrackInfo),
		layerDimensions: make(map[livekit.VideoQuality]*livekit.VideoLayer),
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

	if t.trackInfo.Muted {
		t.SetMuted(true)
	}

	if t.trackInfo != nil && t.Kind() == livekit.TrackType_VIDEO {
		t.UpdateVideoLayers(t.trackInfo.Layers)
		// LK-TODO: maybe use this or simulcast flag in TrackInfo to set simulcasted here
	}

	return t
}

func (t *MediaTrackReceiver) Restart() {
	t.lock.Lock()
	receiver := t.receiver
	t.lock.Unlock()

	if receiver != nil {
		receiver.SetMaxExpectedSpatialLayer(SpatialLayerForQuality(livekit.VideoQuality_HIGH))
		t.MediaTrackSubscriptions.Restart()
	}
}

func (t *MediaTrackReceiver) SetupReceiver(receiver sfu.TrackReceiver) {
	t.lock.Lock()
	t.receiver = receiver
	t.lock.Unlock()

	t.MediaTrackSubscriptions.Start()
}

func (t *MediaTrackReceiver) ClearReceiver() {
	t.lock.Lock()
	t.receiver = nil
	t.lock.Unlock()

	t.MediaTrackSubscriptions.Close()
}

func (t *MediaTrackReceiver) OnMediaLossUpdate(f func(fractionalLoss uint8)) {
	t.onMediaLossUpdate = f
}

func (t *MediaTrackReceiver) OnVideoLayerUpdate(f func(layers []*livekit.VideoLayer)) {
	t.onVideoLayerUpdate = f
}

func (t *MediaTrackReceiver) Close() {
	t.lock.Lock()
	onclose := t.onClose
	t.lock.Unlock()

	t.MediaTrackSubscriptions.Close()

	for _, f := range onclose {
		f()
	}
}

func (t *MediaTrackReceiver) ID() livekit.TrackID {
	t.lock.RLock()
	defer t.lock.RUnlock()

	return livekit.TrackID(t.trackInfo.Sid)
}

func (t *MediaTrackReceiver) Kind() livekit.TrackType {
	t.lock.RLock()
	defer t.lock.RUnlock()

	return t.trackInfo.Type
}

func (t *MediaTrackReceiver) Source() livekit.TrackSource {
	t.lock.RLock()
	defer t.lock.RUnlock()

	return t.trackInfo.Source
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
	t.lock.RLock()
	defer t.lock.RUnlock()

	return t.trackInfo.Name
}

func (t *MediaTrackReceiver) IsMuted() bool {
	return t.muted.Load()
}

func (t *MediaTrackReceiver) SetMuted(muted bool) {
	t.muted.Store(muted)

	receiver := t.Receiver()
	if receiver != nil {
		receiver.SetUpTrackPaused(muted)
	}

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
	receiver := t.Receiver()
	if receiver == nil {
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

	downTrack, err := t.MediaTrackSubscriptions.AddSubscriber(sub, receiver.Codec(), NewWrappedReceiver(receiver, t.ID(), streamId))
	if err != nil {
		return err
	}

	if downTrack != nil {
		if t.Kind() == livekit.TrackType_AUDIO {
			downTrack.AddReceiverReportListener(t.handleMaxLossFeedback)
		}

		if err = receiver.AddDownTrack(downTrack); err != nil {
			logger.Errorw("could not add down track", err, "participant", sub.Identity(), "pID", sub.ID())
		}
	}
	return nil
}

func (t *MediaTrackReceiver) UpdateTrackInfo(ti *livekit.TrackInfo) {
	t.lock.Lock()
	t.params.TrackInfo = ti
	t.lock.Unlock()

	if ti != nil && t.Kind() == livekit.TrackType_VIDEO {
		t.UpdateVideoLayers(ti.Layers)
	}
}

func (t *MediaTrackReceiver) TrackInfo() *livekit.TrackInfo {
	t.lock.RLock()
	defer t.lock.RUnlock()

	return proto.Clone(t.params.TrackInfo).(*livekit.TrackInfo)
}

func (t *MediaTrackReceiver) UpdateVideoLayers(layers []*livekit.VideoLayer) {
	t.lock.Lock()
	for _, layer := range layers {
		t.layerDimensions[layer.Quality] = layer
	}
	t.lock.Unlock()

	t.MediaTrackSubscriptions.UpdateVideoLayers()
	if t.onVideoLayerUpdate != nil {
		t.onVideoLayerUpdate(layers)
	}

	// TODO: this might need to trigger a participant update for clients to pick up dimension change
}

func (t *MediaTrackReceiver) GetVideoLayers() []*livekit.VideoLayer {
	layers := make([]*livekit.VideoLayer, 0)
	t.lock.RLock()
	for _, layer := range t.layerDimensions {
		layers = append(layers, proto.Clone(layer).(*livekit.VideoLayer))
	}
	t.lock.RUnlock()

	return layers
}

// GetQualityForDimension finds the closest quality to use for desired dimensions
// affords a 20% tolerance on dimension
func (t *MediaTrackReceiver) GetQualityForDimension(width, height uint32) livekit.VideoQuality {
	kind := t.Kind()

	t.lock.RLock()
	defer t.lock.RUnlock()

	quality := livekit.VideoQuality_HIGH
	if kind == livekit.TrackType_AUDIO || t.trackInfo.Height == 0 {
		return quality
	}
	origSize := t.trackInfo.Height
	requestedSize := height
	if t.trackInfo.Width < t.trackInfo.Height {
		// for portrait videos
		origSize = t.trackInfo.Width
		requestedSize = width
	}

	// default sizes representing qualities low - high
	layerSizes := []uint32{180, 360, origSize}
	var providedSizes []uint32
	for _, layer := range t.layerDimensions {
		providedSizes = append(providedSizes, layer.Height)
	}
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
	receiver := t.Receiver()
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

	receiver := t.Receiver()
	if receiver != nil {
		receiverInfo := receiver.DebugInfo()
		for k, v := range receiverInfo {
			info[k] = v
		}
	}
	return info
}

func (t *MediaTrackReceiver) Receiver() sfu.TrackReceiver {
	t.lock.RLock()
	defer t.lock.RUnlock()

	return t.receiver
}

func (t *MediaTrackReceiver) OnSubscribedMaxQualityChange(f func(trackID livekit.TrackID, subscribedQualities []*livekit.SubscribedQuality, maxSubscribedQuality livekit.VideoQuality) error) {
	t.MediaTrackSubscriptions.OnSubscribedMaxQualityChange(func(subscribedQualities []*livekit.SubscribedQuality, maxSubscribedQuality livekit.VideoQuality) {
		if f != nil && !t.IsMuted() {
			_ = f(t.ID(), subscribedQualities, maxSubscribedQuality)
		}
		receiver := t.Receiver()
		if receiver != nil {
			receiver.SetMaxExpectedSpatialLayer(SpatialLayerForQuality(maxSubscribedQuality))
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
