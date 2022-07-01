package rtc

import (
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"
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
	receiversShadow []*simulcastReceiver
	trackInfo       *livekit.TrackInfo
	layerDimensions map[livekit.VideoQuality]*livekit.VideoLayer
	potentialCodecs []webrtc.RTPCodecParameters

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
	t.MediaTrackSubscriptions.OnDownTrackCreated(t.onDownTrackCreated)

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
	receivers := t.receiversShadow
	t.lock.Unlock()

	for _, receiver := range receivers {
		receiver.SetMaxExpectedSpatialLayer(utils.SpatialLayerForQuality(livekit.VideoQuality_HIGH))
	}

	t.MediaTrackSubscriptions.Restart()
}

func (t *MediaTrackReceiver) SetupReceiver(receiver sfu.TrackReceiver, priority int, mid string) {
	t.lock.Lock()

	// codec postion maybe taked by DumbReceiver, check and upgrade to WebRTCReceiver
	var upgradeReceiver bool
	for _, r := range t.receivers {
		if strings.EqualFold(r.Codec().MimeType, receiver.Codec().MimeType) {
			if d, ok := r.TrackReceiver.(*DummyReceiver); ok {
				d.Upgrade(receiver)
				upgradeReceiver = true
				break
			}
		}
	}
	if !upgradeReceiver {
		t.receivers = append(t.receivers, &simulcastReceiver{TrackReceiver: receiver, priority: priority})
	}

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
				break
			}
		}
	}

	t.shadowReceiversLocked()

	t.params.Logger.Debugw("setup receiver", "mime", receiver.Codec().MimeType, "priority", priority, "receivers", t.receiversShadow)
	t.lock.Unlock()

	t.MediaTrackSubscriptions.AddCodec(receiver.Codec().MimeType)

	t.MediaTrackSubscriptions.Start()
}

func (t *MediaTrackReceiver) SetPotentialCodecs(codecs []webrtc.RTPCodecParameters, headers []webrtc.RTPHeaderExtensionParameter) {
	t.lock.Lock()
	t.potentialCodecs = codecs
	for i, c := range codecs {
		var exist bool
		for _, r := range t.receivers {
			if strings.EqualFold(c.MimeType, r.Codec().MimeType) {
				exist = true
				break
			}
		}
		if !exist {
			t.receivers = append(t.receivers, &simulcastReceiver{
				TrackReceiver: NewDummyReceiver(livekit.TrackID(t.trackInfo.Sid), string(t.PublisherID()), c, headers),
				priority:      i,
			})
		}
	}
	sort.Slice(t.receivers, func(i, j int) bool {
		return t.receivers[i].Priority() < t.receivers[j].Priority()
	})
	t.shadowReceiversLocked()
	t.lock.Unlock()
}

func (t *MediaTrackReceiver) shadowReceiversLocked() {
	t.receiversShadow = make([]*simulcastReceiver, len(t.receivers))
	copy(t.receiversShadow, t.receivers)
}

func (t *MediaTrackReceiver) SetLayerSsrc(mime string, rid string, ssrc uint32) {
	t.lock.Lock()
	defer t.lock.Unlock()

	layer := sfu.RidToLayer(rid)
	if layer == sfu.InvalidLayerSpatial {
		// non-simulcast case will not have `rid`
		layer = 0
	}
	for _, receiver := range t.receiversShadow {
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

	t.receiversShadow = make([]*simulcastReceiver, len(t.receivers))
	copy(t.receiversShadow, t.receivers)

	stopSubscription := len(t.receiversShadow) == 0
	t.lock.Unlock()

	if stopSubscription {
		t.MediaTrackSubscriptions.Stop()
	}
}

func (t *MediaTrackReceiver) ClearAllReceivers() {
	t.lock.Lock()
	t.receivers = t.receivers[:0]
	t.receiversShadow = nil
	t.lock.Unlock()
	t.MediaTrackSubscriptions.Stop()
}

func (t *MediaTrackReceiver) OnMediaLossUpdate(f func(fractionalLoss uint8)) {
	t.onMediaLossUpdate = f
}

func (t *MediaTrackReceiver) OnVideoLayerUpdate(f func(layers []*livekit.VideoLayer)) {
	t.onVideoLayerUpdate = f
}

func (t *MediaTrackReceiver) TryClose() bool {
	t.lock.RLock()
	if len(t.receiversShadow) > 0 {
		t.lock.Unlock()
		return false
	}
	t.lock.RUnlock()
	t.Close()

	return true
}

func (t *MediaTrackReceiver) Close() {
	t.lock.RLock()
	onclose := t.onClose
	t.lock.RUnlock()

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

	t.lock.RLock()
	receivers := t.receiversShadow
	t.lock.RUnlock()
	for _, receiver := range receivers {
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
	t.lock.RLock()
	receivers := t.receiversShadow
	potentialCodecs := make([]webrtc.RTPCodecParameters, len(t.potentialCodecs))
	copy(potentialCodecs, t.potentialCodecs)
	t.lock.RUnlock()

	if len(receivers) == 0 {
		// cannot add, no receiver
		return errors.New("cannot subscribe without a receiver in place")
	}

	for _, receiver := range receivers {
		codec := receiver.Codec()
		var found bool
		for _, pc := range potentialCodecs {
			if codec.MimeType == pc.MimeType {
				found = true
				break
			}
		}
		if !found {
			potentialCodecs = append(potentialCodecs, codec)
		}
	}

	// using DownTrack from ion-sfu
	streamId := string(t.PublisherID())
	if sub.ProtocolVersion().SupportsPackedStreamId() {
		// when possible, pack both IDs in streamID to allow new streams to be generated
		// react-native-webrtc still uses stream based APIs and require this
		streamId = PackStreamID(t.PublisherID(), t.ID())
	}

	err := t.MediaTrackSubscriptions.AddSubscriber(sub, NewWrappedReceiver(receivers, t.ID(), streamId, potentialCodecs))
	if err != nil {
		return err
	}

	return nil
}

func (t *MediaTrackReceiver) UpdateTrackInfo(ti *livekit.TrackInfo) {
	t.lock.Lock()
	t.trackInfo = proto.Clone(ti).(*livekit.TrackInfo)
	t.lock.Unlock()

	if ti != nil && t.Kind() == livekit.TrackType_VIDEO {
		t.UpdateVideoLayers(ti.Layers)
	}
}

func (t *MediaTrackReceiver) TrackInfo(generateLayer bool) *livekit.TrackInfo {
	t.lock.RLock()
	defer t.lock.RUnlock()

	ti := proto.Clone(t.trackInfo).(*livekit.TrackInfo)
	if !generateLayer {
		return ti
	}

	layers := t.getVideoLayersLocked()

	// set video layer ssrc info
	for i, ci := range ti.Codecs {
		for _, receiver := range t.receiversShadow {
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
	if len(ti.Codecs) == 0 && len(t.receiversShadow) > 0 {
		receiver := t.receiversShadow[0]
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
	t.lock.RLock()
	defer t.lock.RUnlock()

	return t.getVideoLayersLocked()
}

func (t *MediaTrackReceiver) getVideoLayersLocked() []*livekit.VideoLayer {
	layers := make([]*livekit.VideoLayer, 0)
	for _, layer := range t.layerDimensions {
		layers = append(layers, proto.Clone(layer).(*livekit.VideoLayer))
	}

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

func (t *MediaTrackReceiver) onDownTrackCreated(downTrack *sfu.DownTrack) {
	if t.Kind() == livekit.TrackType_AUDIO {
		downTrack.AddReceiverReportListener(t.handleMaxLossFeedback)
	}
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
	receivers := t.receiversShadow
	t.lock.RUnlock()
	for _, receiver := range receivers {
		info[receiver.Codec().MimeType] = receiver.DebugInfo()
	}

	return info
}

func (t *MediaTrackReceiver) PrimaryReceiver() sfu.TrackReceiver {
	t.lock.RLock()
	defer t.lock.RUnlock()

	if len(t.receiversShadow) == 0 {
		return nil
	}
	if dr, ok := t.receiversShadow[0].TrackReceiver.(*DummyReceiver); ok {
		return dr.Receiver()
	}
	return t.receiversShadow[0].TrackReceiver
}

func (t *MediaTrackReceiver) Receiver(mime string) sfu.TrackReceiver {
	t.lock.RLock()
	defer t.lock.RUnlock()

	for _, r := range t.receiversShadow {
		if strings.EqualFold(r.Codec().MimeType, mime) {
			if dr, ok := r.TrackReceiver.(*DummyReceiver); ok {
				return dr.Receiver()
			}
			return r.TrackReceiver
		}
	}
	return nil
}

func (t *MediaTrackReceiver) Receivers() []sfu.TrackReceiver {
	t.lock.RLock()
	defer t.lock.RUnlock()

	receivers := make([]sfu.TrackReceiver, 0, len(t.receiversShadow))
	for _, r := range t.receiversShadow {
		receivers = append(receivers, r.TrackReceiver)
	}
	return receivers
}

func (t *MediaTrackReceiver) SetRTT(rtt uint32) {
	t.lock.Lock()
	receivers := t.receiversShadow
	t.lock.Unlock()

	for _, r := range receivers {
		if wr, ok := r.TrackReceiver.(*sfu.WebRTCReceiver); ok {
			wr.SetRTT(rtt)
		}
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
