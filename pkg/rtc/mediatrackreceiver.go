package rtc

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"
	"go.uber.org/atomic"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/sfu"
	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/telemetry"
)

const (
	layerSelectionTolerance = 0.9
)

var (
	ErrNotOpen    = errors.New("track is not open")
	ErrNoReceiver = errors.New("cannot subscribe without a receiver in place")
)

// ------------------------------------------------------

type mediaTrackReceiverState int

const (
	mediaTrackReceiverStateOpen mediaTrackReceiverState = iota
	mediaTrackReceiverStateClosed
)

func (m mediaTrackReceiverState) String() string {
	switch m {
	case mediaTrackReceiverStateOpen:
		return "OPEN"
	case mediaTrackReceiverStateClosed:
		return "CLOSED"
	default:
		return fmt.Sprintf("%d", int(m))
	}
}

//-----------------------------------------------------

type simulcastReceiver struct {
	sfu.TrackReceiver
	priority   int
	layerSSRCs [livekit.VideoQuality_HIGH + 1]uint32
}

func (r *simulcastReceiver) Priority() int {
	return r.priority
}

type MediaTrackReceiverParams struct {
	TrackInfo           *livekit.TrackInfo
	MediaTrack          types.MediaTrack
	IsRelayed           bool
	ParticipantID       livekit.ParticipantID
	ParticipantIdentity livekit.ParticipantIdentity
	ParticipantVersion  uint32
	BufferFactory       *buffer.Factory
	ReceiverConfig      ReceiverConfig
	SubscriberConfig    DirectionConfig
	Telemetry           telemetry.TelemetryService
	Logger              logger.Logger
}

type MediaTrackReceiver struct {
	params      MediaTrackReceiverParams
	muted       atomic.Bool
	simulcasted atomic.Bool

	lock               sync.RWMutex
	receivers          []*simulcastReceiver
	receiversShadow    []*simulcastReceiver
	trackInfo          *livekit.TrackInfo
	layerDimensions    map[livekit.VideoQuality]*livekit.VideoLayer
	potentialCodecs    []webrtc.RTPCodecParameters
	pendingSubscribeOp map[livekit.ParticipantID]int
	state              mediaTrackReceiverState

	onSetupReceiver     func(mime string)
	onMediaLossFeedback func(dt *sfu.DownTrack, report *rtcp.ReceiverReport)
	onVideoLayerUpdate  func(layers []*livekit.VideoLayer)
	onClose             []func()

	*MediaTrackSubscriptions
}

func NewMediaTrackReceiver(params MediaTrackReceiverParams) *MediaTrackReceiver {
	t := &MediaTrackReceiver{
		params:             params,
		trackInfo:          proto.Clone(params.TrackInfo).(*livekit.TrackInfo),
		layerDimensions:    make(map[livekit.VideoQuality]*livekit.VideoLayer),
		pendingSubscribeOp: make(map[livekit.ParticipantID]int),
		state:              mediaTrackReceiverStateOpen,
	}

	t.MediaTrackSubscriptions = NewMediaTrackSubscriptions(MediaTrackSubscriptionsParams{
		MediaTrack:       params.MediaTrack,
		IsRelayed:        params.IsRelayed,
		BufferFactory:    params.BufferFactory,
		ReceiverConfig:   params.ReceiverConfig,
		SubscriberConfig: params.SubscriberConfig,
		Telemetry:        params.Telemetry,
		Logger:           params.Logger,
	})
	t.MediaTrackSubscriptions.OnDownTrackCreated(t.onDownTrackCreated)
	t.MediaTrackSubscriptions.OnSubscriptionOperationComplete(func(sub types.LocalParticipant) {
		t.removePendingSubscribeOp(sub.ID())
		sub.ClearInProgressAndProcessSubscriptionRequestsQueue(t.ID())
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
	receivers := t.receiversShadow
	t.lock.Unlock()

	for _, receiver := range receivers {
		receiver.SetMaxExpectedSpatialLayer(buffer.VideoQualityToSpatialLayer(livekit.VideoQuality_HIGH, t.params.TrackInfo))
	}
}

func (t *MediaTrackReceiver) OnSetupReceiver(f func(mime string)) {
	t.lock.Lock()
	t.onSetupReceiver = f
	t.lock.Unlock()
}

func (t *MediaTrackReceiver) SetupReceiver(receiver sfu.TrackReceiver, priority int, mid string) {
	t.lock.Lock()
	if t.state != mediaTrackReceiverStateOpen {
		t.params.Logger.Warnw("cannot set up receiver on a track not open", nil)
		t.lock.Unlock()
		return
	}

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

	onSetupReceiver := t.onSetupReceiver
	t.params.Logger.Debugw("setup receiver", "mime", receiver.Codec().MimeType, "priority", priority, "receivers", t.receiversShadow)
	t.lock.Unlock()

	if onSetupReceiver != nil {
		onSetupReceiver(receiver.Codec().MimeType)
	}
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

	layer := buffer.RidToSpatialLayer(rid, t.params.TrackInfo)
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

func (t *MediaTrackReceiver) ClearReceiver(mime string, willBeResumed bool) {
	t.params.Logger.Debugw("clearing receiver", "mime", mime)
	t.lock.Lock()
	for idx, receiver := range t.receivers {
		if strings.EqualFold(receiver.Codec().MimeType, mime) {
			t.receivers[idx] = t.receivers[len(t.receivers)-1]
			t.receivers = t.receivers[:len(t.receivers)-1]
			break
		}
	}

	t.shadowReceiversLocked()
	t.lock.Unlock()

	t.removeAllSubscribersForMime(mime, willBeResumed)
}

func (t *MediaTrackReceiver) ClearAllReceivers(willBeResumed bool) {
	t.params.Logger.Debugw("clearing all receivers")
	t.lock.Lock()
	var mimes []string
	for _, receiver := range t.receivers {
		mimes = append(mimes, receiver.Codec().MimeType)
	}

	t.receivers = t.receivers[:0]
	t.receiversShadow = nil
	t.lock.Unlock()

	for _, mime := range mimes {
		t.ClearReceiver(mime, willBeResumed)
	}
}

func (t *MediaTrackReceiver) OnMediaLossFeedback(f func(dt *sfu.DownTrack, rr *rtcp.ReceiverReport)) {
	t.onMediaLossFeedback = f
}

func (t *MediaTrackReceiver) OnVideoLayerUpdate(f func(layers []*livekit.VideoLayer)) {
	t.onVideoLayerUpdate = f
}

func (t *MediaTrackReceiver) TryClose() bool {
	t.lock.RLock()
	if t.state == mediaTrackReceiverStateClosed {
		t.lock.RUnlock()
		return true
	}

	for _, receiver := range t.receiversShadow {
		if dr, _ := receiver.TrackReceiver.(*DummyReceiver); dr != nil && dr.Receiver() != nil {
			t.lock.RUnlock()
			return false
		}
	}
	t.lock.RUnlock()
	t.Close()

	return true
}

func (t *MediaTrackReceiver) Close() {
	t.lock.Lock()
	if t.state == mediaTrackReceiverStateClosed {
		t.lock.Unlock()
		return
	}

	t.state = mediaTrackReceiverStateClosed
	onclose := t.onClose
	t.lock.Unlock()

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

func (t *MediaTrackReceiver) PublisherVersion() uint32 {
	return t.params.ParticipantVersion
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

func (t *MediaTrackReceiver) addPendingSubscribeOp(subscriberID livekit.ParticipantID) {
	t.lock.Lock()
	if c, ok := t.pendingSubscribeOp[subscriberID]; !ok {
		t.pendingSubscribeOp[subscriberID] = 1
	} else {
		t.pendingSubscribeOp[subscriberID] = c + 1
	}
	t.lock.Unlock()
}

func (t *MediaTrackReceiver) removePendingSubscribeOp(subscriberID livekit.ParticipantID) {
	t.lock.Lock()
	if c, ok := t.pendingSubscribeOp[subscriberID]; ok {
		t.pendingSubscribeOp[subscriberID] = c - 1
		if t.pendingSubscribeOp[subscriberID] == 0 {
			delete(t.pendingSubscribeOp, subscriberID)
		}
	}
	t.lock.Unlock()
}

// AddSubscriber subscribes sub to current mediaTrack
func (t *MediaTrackReceiver) AddSubscriber(sub types.LocalParticipant) error {
	t.addPendingSubscribeOp(sub.ID())

	trackID := t.ID()
	sub.EnqueueSubscribeTrack(trackID, t.params.IsRelayed, t.addSubscriber)
	return nil
}

func (t *MediaTrackReceiver) addSubscriber(sub types.LocalParticipant) (err error) {
	defer func() {
		if err != nil {
			t.removePendingSubscribeOp(sub.ID())
		}
	}()

	t.lock.RLock()
	if t.state != mediaTrackReceiverStateOpen {
		t.lock.RUnlock()
		err = ErrNotOpen
		return
	}

	receivers := t.receiversShadow
	potentialCodecs := make([]webrtc.RTPCodecParameters, len(t.potentialCodecs))
	copy(potentialCodecs, t.potentialCodecs)
	t.lock.RUnlock()

	if len(receivers) == 0 {
		// cannot add, no receiver
		err = ErrNoReceiver
		return
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

	tLogger := LoggerWithTrack(sub.GetLogger(), t.ID(), t.params.IsRelayed)
	err = t.MediaTrackSubscriptions.AddSubscriber(sub, NewWrappedReceiver(WrappedReceiverParams{
		Receivers:      receivers,
		TrackID:        t.ID(),
		StreamId:       streamId,
		UpstreamCodecs: potentialCodecs,
		Logger:         tLogger,
		DisableRed:     t.trackInfo.GetDisableRed(),
	}))
	if err != nil {
		return
	}

	return nil
}

// RemoveSubscriber removes participant from subscription
// stop all forwarders to the client
func (t *MediaTrackReceiver) RemoveSubscriber(subscriberID livekit.ParticipantID, willBeResumed bool) {
	subTrack := t.getSubscribedTrack(subscriberID)
	if subTrack == nil {
		return
	}

	sub := subTrack.Subscriber()
	t.addPendingSubscribeOp(sub.ID())

	sub.EnqueueUnsubscribeTrack(subTrack.ID(), t.params.IsRelayed, willBeResumed, t.removeSubscriber)
}

func (t *MediaTrackReceiver) removeSubscriber(subscriberID livekit.ParticipantID, willBeResumed bool) (err error) {
	defer func() {
		if err != nil {
			t.removePendingSubscribeOp(subscriberID)
		}
	}()

	err = t.MediaTrackSubscriptions.RemoveSubscriber(subscriberID, willBeResumed)
	return
}

func (t *MediaTrackReceiver) removeAllSubscribersForMime(mime string, willBeResumed bool) {
	t.params.Logger.Infow("removing all subscribers for mime", "mime", mime)
	for _, subscriberID := range t.MediaTrackSubscriptions.GetAllSubscribersForMime(mime) {
		t.RemoveSubscriber(subscriberID, willBeResumed)
	}
}

func (t *MediaTrackReceiver) IsSubscribed() bool {
	t.lock.RLock()
	defer t.lock.RUnlock()

	return t.MediaTrackSubscriptions.GetNumSubscribers() != 0 || len(t.pendingSubscribeOp) != 0
}

func (t *MediaTrackReceiver) RevokeDisallowedSubscribers(allowedSubscriberIdentities []livekit.ParticipantIdentity) []livekit.ParticipantIdentity {
	var revokedSubscriberIdentities []livekit.ParticipantIdentity

	// LK-TODO: large number of subscribers needs to be solved for this loop
	for _, subTrack := range t.MediaTrackSubscriptions.getAllSubscribedTracks() {
		found := false
		for _, allowedIdentity := range allowedSubscriberIdentities {
			if subTrack.SubscriberIdentity() == allowedIdentity {
				found = true
				break
			}
		}

		if !found {
			t.params.Logger.Infow("revoking subscription",
				"subscriber", subTrack.SubscriberIdentity(),
				"subscriberID", subTrack.SubscriberID(),
			)
			t.RemoveSubscriber(subTrack.SubscriberID(), false)
			revokedSubscriberIdentities = append(revokedSubscriberIdentities, subTrack.SubscriberIdentity())
		}
	}

	return revokedSubscriberIdentities
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
					ssrcFound := false
					for _, l := range originLayers {
						if l.Quality == ci.Layers[layerIdx].Quality {
							if l.Ssrc != 0 {
								ci.Layers[layerIdx].Ssrc = l.Ssrc
								ssrcFound = true
							}
							break
						}
					}
					if !ssrcFound && int(layer.Quality) < len(receiver.layerSSRCs) {
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
			ssrcFound := false
			for _, l := range originLayers {
				if l.Quality == ti.Layers[layerIdx].Quality {
					if l.Ssrc != 0 {
						ti.Layers[layerIdx].Ssrc = l.Ssrc
						ssrcFound = true
					}
					break
				}
			}
			if !ssrcFound && int(layer.Quality) < len(receiver.layerSSRCs) {
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
		downTrack.AddReceiverReportListener(func(dt *sfu.DownTrack, rr *rtcp.ReceiverReport) {
			if t.onMediaLossFeedback != nil {
				t.onMediaLossFeedback(dt, rr)
			}
		})
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

func (t *MediaTrackReceiver) GetTemporalLayerForSpatialFps(spatial int32, fps uint32, mime string) int32 {
	receiver := t.Receiver(mime)
	if receiver == nil {
		return buffer.DefaultMaxLayerTemporal
	}

	layerFps := receiver.GetTemporalLayerFpsForSpatial(spatial)
	requestFps := float32(fps) * layerSelectionTolerance
	for i, f := range layerFps {
		if requestFps <= f {
			return int32(i)
		}
	}
	return buffer.DefaultMaxLayerTemporal
}

// ---------------------------
