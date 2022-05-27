package rtc

import (
	"errors"
	"sort"
	"strings"
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
	"github.com/livekit/livekit-server/pkg/utils"
)

const (
	downLostUpdateDelta     = time.Second
	layerSelectionTolerance = 0.9
)

type simulcastReceiver struct {
	sfu.TrackReceiver
	priority   int
	layerSSRCs [livekit.VideoQuality_HIGH + 1]uint32
}

func (r *simulcastReceiver) Priority() int {
	return r.priority
}

type MediaTrackReceiver struct {
	params      MediaTrackReceiverParams
	muted       atomic.Bool
	simulcasted atomic.Bool

	lock            sync.RWMutex
	receivers       []*simulcastReceiver
	trackInfo       *livekit.TrackInfo
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
	receivers := t.receivers
	t.lock.Unlock()

	for _, receiver := range receivers {
		receiver.SetMaxExpectedSpatialLayer(utils.SpatialLayerForQuality(livekit.VideoQuality_HIGH))
	}

	t.MediaTrackSubscriptions.Restart()
}

func (t *MediaTrackReceiver) SetupReceiver(receiver sfu.TrackReceiver, priority int, mid string) {
	t.lock.Lock()
	t.receivers = append(t.receivers, &simulcastReceiver{TrackReceiver: receiver, priority: priority})
	sort.Slice(t.receivers, func(i, j int) bool {
		return t.receivers[i].Priority() < t.receivers[j].Priority()
	})

	if mid != "" {
		if priority == 0 {
			t.trackInfo.MimeType = receiver.Codec().MimeType
			t.trackInfo.Mid = mid

			// for clients don't have simulcast codecs (old version or single codec), add the primary codec
			if len(t.trackInfo.Codecs) == 0 && t.trackInfo.Type == livekit.TrackType_VIDEO {
				t.trackInfo.Codecs = append(t.trackInfo.Codecs, &livekit.SimulcastCodecInfo{})
			}
		}

		for i, ci := range t.trackInfo.Codecs {
			if i == priority {
				ci.Mid = mid
				ci.MimeType = receiver.Codec().MimeType
			}
		}
	}

	t.lock.Unlock()
	t.params.Logger.Debugw("setup receiver", "mime", receiver.Codec().MimeType, "priority", priority, "receivers", t.receivers)
	t.MediaTrackSubscriptions.AddCodec(receiver.Codec().MimeType)

	t.MediaTrackSubscriptions.Start()
}

func (t *MediaTrackReceiver) SetLayerSsrc(mime string, rid string, ssrc uint32) {
	t.lock.Lock()
	defer t.lock.Unlock()

	layer := sfu.RidToLayer(rid)
	for _, receiver := range t.receivers {
		if strings.EqualFold(receiver.Codec().MimeType, mime) && int(layer) < len(receiver.layerSSRCs) {
			receiver.layerSSRCs[layer] = ssrc
			return
		}
	}
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
	t.lock.Lock()
	t.params.TrackInfo = ti
	t.trackInfo = proto.Clone(ti).(*livekit.TrackInfo)
	t.lock.Unlock()

	if ti != nil && t.Kind() == livekit.TrackType_VIDEO {
		t.UpdateVideoLayers(ti.Layers)
	}
}

func (t *MediaTrackReceiver) TrackInfo(generateLayer bool) *livekit.TrackInfo {
	t.lock.RLock()
	ti := proto.Clone(t.trackInfo).(*livekit.TrackInfo)
	t.lock.RUnlock()
	if !generateLayer {
		return ti
	}
	layers := t.GetVideoLayers()

	// set video layer ssrc info
	for i, ci := range ti.Codecs {
		for _, receiver := range t.receivers {
			if receiver.priority == i {
				originLayers := ci.Layers
				ci.Layers = []*livekit.VideoLayer{}
				for layerIdx, layer := range layers {
					ci.Layers = append(ci.Layers, proto.Clone(layer).(*livekit.VideoLayer))

					// if origin layer has ssrc, don't override it
					if layerIdx < len(originLayers) && originLayers[layerIdx].Ssrc != 0 {
						ci.Layers[layerIdx].Ssrc = originLayers[layerIdx].Ssrc
					} else if int(layer.Quality) < len(receiver.layerSSRCs) {
						ci.Layers[layerIdx].Ssrc = receiver.layerSSRCs[layer.Quality]
					}
				}

				if i == 0 {
					ti.Layers = ci.Layers
				}
				break
			}
		}
	}

	// for client don't use simulcast codecs (old client version or single codec)
	if len(ti.Codecs) == 0 && len(t.receivers) > 0 {
		receiver := t.receivers[0]
		originLayers := ti.Layers
		ti.Layers = []*livekit.VideoLayer{}
		for layerIdx, layer := range layers {
			ti.Layers = append(ti.Layers, proto.Clone(layer).(*livekit.VideoLayer))

			// if origin layer has ssrc, don't override it
			if layerIdx < len(originLayers) && originLayers[layerIdx].Ssrc != 0 {
				ti.Layers[layerIdx].Ssrc = originLayers[layerIdx].Ssrc
			} else if int(layer.Quality) < len(receiver.layerSSRCs) {
				ti.Layers[layerIdx].Ssrc = receiver.layerSSRCs[layer.Quality]
			}
		}
	}

	return ti
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
	quality := livekit.VideoQuality_HIGH
	if t.Kind() == livekit.TrackType_AUDIO {
		return quality
	}

	t.lock.RLock()
	defer t.lock.RUnlock()

	if t.trackInfo.Height == 0 {
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

func (t *MediaTrackReceiver) Receivers() []sfu.TrackReceiver {
	t.lock.RLock()
	defer t.lock.RUnlock()

	receivers := make([]sfu.TrackReceiver, 0, len(t.receivers))
	for _, r := range t.receivers {
		receivers = append(receivers, r.TrackReceiver)
	}
	return receivers
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
				receiver.SetMaxExpectedSpatialLayer(utils.SpatialLayerForQuality(q.Quality))
			}
		}
	})
}

// ---------------------------

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
