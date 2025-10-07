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
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
	"go.uber.org/atomic"
	"golang.org/x/exp/slices"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils"

	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/sfu"
	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/sfu/mime"
	"github.com/livekit/livekit-server/pkg/sfu/rtpstats"
	"github.com/livekit/livekit-server/pkg/telemetry"
	sutils "github.com/livekit/livekit-server/pkg/utils"
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
	mediaTrackReceiverStateClosing
	mediaTrackReceiverStateClosed
)

func (m mediaTrackReceiverState) String() string {
	switch m {
	case mediaTrackReceiverStateOpen:
		return "OPEN"
	case mediaTrackReceiverStateClosing:
		return "CLOSING"
	case mediaTrackReceiverStateClosed:
		return "CLOSED"
	default:
		return fmt.Sprintf("%d", int(m))
	}
}

// -----------------------------------------------------

type simulcastReceiver struct {
	sfu.TrackReceiver
	priority  int
	lock      sync.Mutex
	regressTo sfu.TrackReceiver
}

func (r *simulcastReceiver) Priority() int {
	return r.priority
}

func (r *simulcastReceiver) AddDownTrack(track sfu.TrackSender) error {
	r.lock.Lock()
	if rt := r.regressTo; rt != nil {
		r.lock.Unlock()
		// AddDownTrack could be called in downtrack.OnBinding callback, use a goroutine to avoid deadlock
		go track.SetReceiver(rt)
		return nil
	}
	err := r.TrackReceiver.AddDownTrack(track)
	r.lock.Unlock()
	return err
}

func (r *simulcastReceiver) RegressTo(receiver sfu.TrackReceiver) {
	r.lock.Lock()
	r.regressTo = receiver
	dts := r.GetDownTracks()
	r.lock.Unlock()

	for _, dt := range dts {
		dt.SetReceiver(receiver)
	}
}

func (r *simulcastReceiver) IsRegressed() bool {
	r.lock.Lock()
	defer r.lock.Unlock()
	return r.regressTo != nil
}

// -----------------------------------------------------

type MediaTrackReceiverParams struct {
	MediaTrack               types.MediaTrack
	IsRelayed                bool
	ParticipantID            func() livekit.ParticipantID
	ParticipantIdentity      livekit.ParticipantIdentity
	ParticipantVersion       uint32
	ReceiverConfig           ReceiverConfig
	SubscriberConfig         DirectionConfig
	AudioConfig              sfu.AudioConfig
	Telemetry                telemetry.TelemetryService
	Logger                   logger.Logger
	RegressionTargetCodec    mime.MimeType
	PreferVideoSizeFromMedia bool
}

type MediaTrackReceiver struct {
	params MediaTrackReceiverParams

	lock               sync.RWMutex
	receivers          []*simulcastReceiver
	trackInfo          atomic.Pointer[livekit.TrackInfo]
	potentialCodecs    []webrtc.RTPCodecParameters
	state              mediaTrackReceiverState
	isExpectedToResume bool

	onSetupReceiver     func(mime mime.MimeType)
	onMediaLossFeedback func(dt *sfu.DownTrack, report *rtcp.ReceiverReport)
	onClose             []func(isExpectedToResume bool)
	onCodecRegression   func(old, new webrtc.RTPCodecParameters)

	*MediaTrackSubscriptions
}

func NewMediaTrackReceiver(params MediaTrackReceiverParams, ti *livekit.TrackInfo) *MediaTrackReceiver {
	t := &MediaTrackReceiver{
		params: params,
		state:  mediaTrackReceiverStateOpen,
	}
	t.trackInfo.Store(utils.CloneProto(ti))

	t.MediaTrackSubscriptions = NewMediaTrackSubscriptions(MediaTrackSubscriptionsParams{
		MediaTrack:       params.MediaTrack,
		IsRelayed:        params.IsRelayed,
		ReceiverConfig:   params.ReceiverConfig,
		SubscriberConfig: params.SubscriberConfig,
		Telemetry:        params.Telemetry,
		Logger:           params.Logger,
	})
	t.MediaTrackSubscriptions.OnDownTrackCreated(t.onDownTrackCreated)
	return t
}

func (t *MediaTrackReceiver) Restart() {
	for _, receiver := range t.loadReceivers() {
		hq := buffer.GetSpatialLayerForVideoQuality(receiver.Mime(), livekit.VideoQuality_HIGH, t.TrackInfo())
		receiver.SetMaxExpectedSpatialLayer(hq)
	}
}

func (t *MediaTrackReceiver) OnSetupReceiver(f func(mime mime.MimeType)) {
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

	receivers := slices.Clone(t.receivers)

	// codec position maybe taken by DummyReceiver, check and upgrade to WebRTCReceiver
	var existingReceiver bool
	for _, r := range receivers {
		if r.Mime() == receiver.Mime() {
			existingReceiver = true
			if d, ok := r.TrackReceiver.(*DummyReceiver); ok {
				d.Upgrade(receiver)
			} else {
				t.params.Logger.Errorw("receiver already exists, setup failed", nil, "mime", receiver.Mime())
			}
			break
		}
	}
	if !existingReceiver {
		receivers = append(receivers, &simulcastReceiver{TrackReceiver: receiver, priority: priority})
	}

	sort.Slice(receivers, func(i, j int) bool {
		return receivers[i].Priority() < receivers[j].Priority()
	})

	if mid != "" {
		trackInfo := t.TrackInfoClone()
		if priority == 0 {
			trackInfo.MimeType = receiver.Mime().String()
			trackInfo.Mid = mid
		}

		for i, ci := range trackInfo.Codecs {
			if i == priority {
				ci.MimeType = receiver.Mime().String()
				ci.Mid = mid
				break
			}
		}
		t.trackInfo.Store(trackInfo)
	}

	t.receivers = receivers
	onSetupReceiver := t.onSetupReceiver
	t.lock.Unlock()

	var receiverCodecs []string
	for _, r := range receivers {
		receiverCodecs = append(receiverCodecs, r.Mime().String())
	}
	t.params.Logger.Debugw(
		"setup receiver",
		"mime", receiver.Mime(),
		"priority", priority,
		"receivers", receiverCodecs,
		"mid", mid,
	)

	if onSetupReceiver != nil {
		onSetupReceiver(receiver.Mime())
	}
}

func (t *MediaTrackReceiver) HandleReceiverCodecChange(r sfu.TrackReceiver, codec webrtc.RTPCodecParameters, state sfu.ReceiverCodecState) {
	// TODO: we only support codec regress to backup codec now, so the receiver will not be available
	// once fallback / regression happens.
	// We will support codec upgrade in the future then the primary receiver will be available again if
	// all subscribers of the track negotiate it.
	if state == sfu.ReceiverCodecStateNormal {
		return
	}

	t.lock.Lock()
	// codec regression, find backup codec and switch all downtracks to it
	var (
		oldReceiver         *simulcastReceiver
		backupCodecReceiver sfu.TrackReceiver
	)
	for _, receiver := range t.receivers {
		if receiver.TrackReceiver == r {
			oldReceiver = receiver
			continue
		}
		if d, ok := receiver.TrackReceiver.(*DummyReceiver); ok && d.Receiver() == r {
			oldReceiver = receiver
			continue
		}

		if receiver.Mime() == t.params.RegressionTargetCodec {
			backupCodecReceiver = receiver.TrackReceiver
		}

		if oldReceiver != nil && backupCodecReceiver != nil {
			break
		}
	}

	if oldReceiver == nil {
		// should not happen
		t.params.Logger.Errorw("could not find primary receiver for codec", nil, "codec", codec.MimeType)
		t.lock.Unlock()
		return
	}

	if oldReceiver.IsRegressed() {
		t.params.Logger.Infow("codec already regressed", "codec", codec.MimeType)
		t.lock.Unlock()
		return
	}

	if backupCodecReceiver == nil {
		t.params.Logger.Infow("no backup codec found, can't regress codec")
		t.lock.Unlock()
		return
	}

	t.params.Logger.Infow(
		"regressing codec",
		"from", codec.MimeType,
		"to", backupCodecReceiver.Codec().MimeType,
	)

	// remove old codec from potential codecs
	for i, c := range t.potentialCodecs {
		if strings.EqualFold(c.MimeType, codec.MimeType) {
			slices.Delete(t.potentialCodecs, i, i+1)
			break
		}
	}
	onCodecRegression := t.onCodecRegression
	t.lock.Unlock()
	oldReceiver.RegressTo(backupCodecReceiver)

	if onCodecRegression != nil {
		onCodecRegression(codec, backupCodecReceiver.Codec())
	}
}

func (t *MediaTrackReceiver) OnCodecRegression(f func(old, new webrtc.RTPCodecParameters)) {
	t.onCodecRegression = f
}

func (t *MediaTrackReceiver) SetPotentialCodecs(codecs []webrtc.RTPCodecParameters, headers []webrtc.RTPHeaderExtensionParameter) {
	// The potential codecs have not published yet, so we can't get the actual Extensions, the client/browser uses same extensions
	// for all video codecs so we assume they will have same extensions as the primary codec.
	t.lock.Lock()
	receivers := slices.Clone(t.receivers)
	t.potentialCodecs = codecs
	for i, c := range codecs {
		var exist bool
		for _, r := range receivers {
			if mime.NormalizeMimeType(c.MimeType) == r.Mime() {
				exist = true
				break
			}
		}
		if !exist {
			receivers = append(receivers, &simulcastReceiver{
				TrackReceiver: NewDummyReceiver(t.ID(), string(t.PublisherID()), c, headers),
				priority:      i,
			})
		}
	}
	sort.Slice(receivers, func(i, j int) bool {
		return receivers[i].Priority() < receivers[j].Priority()
	})
	t.receivers = receivers
	t.lock.Unlock()
}

func (t *MediaTrackReceiver) ClearReceiver(mime mime.MimeType, isExpectedToResume bool) {
	t.lock.Lock()
	receivers := slices.Clone(t.receivers)
	for idx, receiver := range receivers {
		if receiver.Mime() == mime {
			receivers[idx] = receivers[len(receivers)-1]
			receivers[len(receivers)-1] = nil
			receivers = receivers[:len(receivers)-1]
			break
		}
	}
	t.receivers = receivers
	t.lock.Unlock()

	t.removeAllSubscribersForMime(mime, isExpectedToResume)
}

func (t *MediaTrackReceiver) ClearAllReceivers(isExpectedToResume bool) {
	t.params.Logger.Debugw("clearing all receivers", "isExpectedToResume", isExpectedToResume)
	t.lock.Lock()
	receivers := t.receivers
	t.receivers = nil

	t.isExpectedToResume = isExpectedToResume
	t.lock.Unlock()

	for _, r := range receivers {
		t.removeAllSubscribersForMime(r.Mime(), isExpectedToResume)
	}
}

func (t *MediaTrackReceiver) OnMediaLossFeedback(f func(dt *sfu.DownTrack, rr *rtcp.ReceiverReport)) {
	t.onMediaLossFeedback = f
}

func (t *MediaTrackReceiver) IsOpen() bool {
	t.lock.RLock()
	defer t.lock.RUnlock()
	if t.state != mediaTrackReceiverStateOpen {
		return false
	}
	// If any one of the receivers has entered closed state, we would not consider the track open
	for _, receiver := range t.receivers {
		if receiver.IsClosed() {
			return false
		}
	}
	return true
}

func (t *MediaTrackReceiver) SetClosing(isExpectedToResume bool) {
	t.lock.Lock()
	defer t.lock.Unlock()

	if t.state == mediaTrackReceiverStateOpen {
		t.state = mediaTrackReceiverStateClosing
	}

	t.isExpectedToResume = isExpectedToResume
}

func (t *MediaTrackReceiver) TryClose() bool {
	t.lock.RLock()
	if t.state == mediaTrackReceiverStateClosed {
		t.lock.RUnlock()
		return true
	}

	numActiveReceivers := 0
	for _, receiver := range t.receivers {
		dr, ok := receiver.TrackReceiver.(*DummyReceiver)
		if !ok || dr.Receiver() != nil {
			// !ok means real receiver OR
			// dummy receiver with a regular receiver attached
			numActiveReceivers++
		}
	}

	isExpectedToResume := t.isExpectedToResume
	t.lock.RUnlock()
	if numActiveReceivers != 0 {
		return false
	}

	t.Close(isExpectedToResume)
	return true
}

func (t *MediaTrackReceiver) Close(isExpectedToResume bool) {
	t.ClearAllReceivers(isExpectedToResume)

	t.lock.Lock()
	if t.state == mediaTrackReceiverStateClosed {
		t.lock.Unlock()
		return
	}

	t.state = mediaTrackReceiverStateClosed
	onclose := t.onClose
	t.lock.Unlock()

	for _, f := range onclose {
		f(isExpectedToResume)
	}
}

func (t *MediaTrackReceiver) ID() livekit.TrackID {
	return livekit.TrackID(t.TrackInfo().Sid)
}

func (t *MediaTrackReceiver) Kind() livekit.TrackType {
	return t.TrackInfo().Type
}

func (t *MediaTrackReceiver) Source() livekit.TrackSource {
	return t.TrackInfo().Source
}

func (t *MediaTrackReceiver) Stream() string {
	return t.TrackInfo().Stream
}

func (t *MediaTrackReceiver) PublisherID() livekit.ParticipantID {
	return t.params.ParticipantID()
}

func (t *MediaTrackReceiver) PublisherIdentity() livekit.ParticipantIdentity {
	return t.params.ParticipantIdentity
}

func (t *MediaTrackReceiver) PublisherVersion() uint32 {
	return t.params.ParticipantVersion
}

func (t *MediaTrackReceiver) Name() string {
	return t.TrackInfo().Name
}

func (t *MediaTrackReceiver) IsMuted() bool {
	return t.TrackInfo().Muted
}

func (t *MediaTrackReceiver) SetMuted(muted bool) {
	t.lock.Lock()
	trackInfo := t.TrackInfoClone()
	trackInfo.Muted = muted
	t.trackInfo.Store(trackInfo)

	receivers := t.receivers
	t.lock.Unlock()

	for _, receiver := range receivers {
		receiver.SetUpTrackPaused(muted)
	}

	t.MediaTrackSubscriptions.SetMuted(muted)
}

func (t *MediaTrackReceiver) IsEncrypted() bool {
	return t.TrackInfo().Encryption != livekit.Encryption_NONE
}

func (t *MediaTrackReceiver) AddOnClose(f func(isExpectedToResume bool)) {
	if f == nil {
		return
	}

	t.lock.Lock()
	t.onClose = append(t.onClose, f)
	t.lock.Unlock()
}

// AddSubscriber subscribes sub to current mediaTrack
func (t *MediaTrackReceiver) AddSubscriber(sub types.LocalParticipant) (types.SubscribedTrack, error) {
	t.lock.RLock()
	if t.state != mediaTrackReceiverStateOpen {
		t.lock.RUnlock()
		return nil, ErrNotOpen
	}

	receivers := t.receivers
	potentialCodecs := make([]webrtc.RTPCodecParameters, len(t.potentialCodecs))
	copy(potentialCodecs, t.potentialCodecs)
	t.lock.RUnlock()

	if len(receivers) == 0 {
		// cannot add, no receiver
		return nil, ErrNoReceiver
	}

	for _, receiver := range receivers {
		if receiver.IsRegressed() {
			continue
		}

		codec := receiver.Codec()
		var found bool
		for _, pc := range potentialCodecs {
			if mime.IsMimeTypeStringEqual(codec.MimeType, pc.MimeType) {
				found = true
				break
			}
		}
		if !found {
			potentialCodecs = append(potentialCodecs, codec)
		}
	}

	streamId := string(t.PublisherID())
	if sub.ProtocolVersion().SupportsPackedStreamId() {
		// when possible, pack both IDs in streamID to allow new streams to be generated
		// react-native-webrtc still uses stream based APIs and require this
		streamId = PackStreamID(t.PublisherID(), t.ID())
	}

	tLogger := LoggerWithTrack(sub.GetLogger(), t.ID(), t.params.IsRelayed)
	wr := NewWrappedReceiver(WrappedReceiverParams{
		Receivers:      receivers,
		TrackID:        t.ID(),
		StreamId:       streamId,
		UpstreamCodecs: potentialCodecs,
		Logger:         tLogger,
		DisableRed:     !IsRedEnabled(t.TrackInfo()) || !t.params.AudioConfig.ActiveREDEncoding,
		IsEncrypted:    t.IsEncrypted(),
	})
	subID := sub.ID()
	subTrack, err := t.MediaTrackSubscriptions.AddSubscriber(sub, wr)

	// media track could have been closed while adding subscription
	remove := false
	isExpectedToResume := false
	t.lock.RLock()
	if t.state != mediaTrackReceiverStateOpen {
		isExpectedToResume = t.isExpectedToResume
		remove = true
	}
	t.lock.RUnlock()

	if remove {
		t.params.Logger.Debugw(
			"removing subscriber on a not-open track",
			"subscriberID", subID,
			"isExpectedToResume", isExpectedToResume,
		)
		_ = t.MediaTrackSubscriptions.RemoveSubscriber(subID, isExpectedToResume)
		return nil, ErrNotOpen
	}

	return subTrack, err
}

// RemoveSubscriber removes participant from subscription
// stop all forwarders to the client
func (t *MediaTrackReceiver) RemoveSubscriber(subscriberID livekit.ParticipantID, isExpectedToResume bool) {
	_ = t.MediaTrackSubscriptions.RemoveSubscriber(subscriberID, isExpectedToResume)
}

func (t *MediaTrackReceiver) removeAllSubscribersForMime(mime mime.MimeType, isExpectedToResume bool) {
	t.params.Logger.Debugw("removing all subscribers for mime", "mime", mime, "isExpectedToResume", isExpectedToResume)
	for _, subscriberID := range t.MediaTrackSubscriptions.GetAllSubscribersForMime(mime) {
		t.RemoveSubscriber(subscriberID, isExpectedToResume)
	}
}

func (t *MediaTrackReceiver) RevokeDisallowedSubscribers(allowedSubscriberIdentities []livekit.ParticipantIdentity) []livekit.ParticipantIdentity {
	var revokedSubscriberIdentities []livekit.ParticipantIdentity

	// LK-TODO: large number of subscribers needs to be solved for this loop
	for _, subTrack := range t.MediaTrackSubscriptions.getAllSubscribedTracks() {
		if IsParticipantExemptFromTrackPermissionsRestrictions(subTrack.Subscriber()) {
			continue
		}

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

func (t *MediaTrackReceiver) updateTrackInfoOfReceivers() {
	ti := t.TrackInfo()
	for _, r := range t.loadReceivers() {
		r.UpdateTrackInfo(ti)
	}
}

func (t *MediaTrackReceiver) SetLayerSsrc(mimeType mime.MimeType, rid string, ssrc uint32) {
	t.lock.Lock()
	trackInfo := t.TrackInfoClone()
	layer := buffer.GetSpatialLayerForRid(mimeType, rid, trackInfo)
	if layer == buffer.InvalidLayerSpatial {
		// non-simulcast case will not have `rid`
		layer = 0
	}
	quality := buffer.GetVideoQualityForSpatialLayer(mimeType, layer, trackInfo)
	// set video layer ssrc info
	for i, ci := range trackInfo.Codecs {
		if mime.NormalizeMimeType(ci.MimeType) != mimeType {
			continue
		}

		// if origin layer has ssrc, don't override it
		var matchingLayer *livekit.VideoLayer
		ssrcFound := false
		for _, l := range ci.Layers {
			if l.Quality == quality {
				matchingLayer = l
				if l.Ssrc != 0 {
					ssrcFound = true
				}
				break
			}
		}
		if !ssrcFound && matchingLayer != nil {
			matchingLayer.Ssrc = ssrc
		}

		// for client don't use simulcast codecs (old client version or single codec)
		if i == 0 {
			trackInfo.Layers = ci.Layers
		}
		break
	}
	t.trackInfo.Store(trackInfo)
	t.lock.Unlock()

	t.updateTrackInfoOfReceivers()
}

func (t *MediaTrackReceiver) UpdateCodecInfo(codecs []*livekit.SimulcastCodec) {
	t.lock.Lock()
	trackInfo := t.TrackInfoClone()
	for _, c := range codecs {
		for _, origin := range trackInfo.Codecs {
			if mime.GetMimeTypeCodec(origin.MimeType) == mime.NormalizeMimeTypeCodec(c.Codec) {
				origin.Cid = c.Cid

				if len(c.Layers) != 0 {
					clonedLayers := make([]*livekit.VideoLayer, 0, len(c.Layers))
					for _, l := range c.Layers {
						clonedLayers = append(clonedLayers, utils.CloneProto(l))
					}
					origin.Layers = clonedLayers

					mimeType := mime.NormalizeMimeType(origin.MimeType)
					for _, layer := range origin.Layers {
						layer.SpatialLayer = buffer.VideoQualityToSpatialLayer(mimeType, layer.Quality, trackInfo)
						layer.Rid = buffer.VideoQualityToRid(mimeType, layer.Quality, trackInfo, buffer.DefaultVideoLayersRid)
					}
				}

				break
			}
		}
	}
	t.trackInfo.Store(trackInfo)
	t.lock.Unlock()

	t.updateTrackInfoOfReceivers()
}

func (t *MediaTrackReceiver) UpdateCodecSdpCid(mimeType mime.MimeType, sdpCid string) {
	t.lock.Lock()
	trackInfo := t.TrackInfoClone()
	for _, origin := range trackInfo.Codecs {
		if mime.NormalizeMimeType(origin.MimeType) == mimeType {
			if sdpCid != origin.Cid {
				origin.SdpCid = sdpCid
			}
		}
	}
	t.trackInfo.Store(trackInfo)
	t.lock.Unlock()

	t.updateTrackInfoOfReceivers()
}

func (t *MediaTrackReceiver) UpdateCodecRids(mimeType mime.MimeType, rids buffer.VideoLayersRid) {
	t.lock.Lock()
	trackInfo := t.TrackInfoClone()
	for _, origin := range trackInfo.Codecs {
		originMimeType := mime.NormalizeMimeType(origin.MimeType)
		if originMimeType != mimeType {
			continue
		}

		for _, layer := range origin.Layers {
			layer.SpatialLayer = buffer.VideoQualityToSpatialLayer(mimeType, layer.Quality, trackInfo)
			layer.Rid = buffer.VideoQualityToRid(mimeType, layer.Quality, trackInfo, rids)
		}
		break
	}
	t.trackInfo.Store(trackInfo)
	t.lock.Unlock()

	t.updateTrackInfoOfReceivers()
}

func (t *MediaTrackReceiver) UpdateTrackInfo(ti *livekit.TrackInfo) {
	updateMute := false
	clonedInfo := utils.CloneProto(ti)

	t.lock.Lock()
	trackInfo := t.TrackInfo()
	// patch Mid and SSRC of codecs/layers by keeping original if available
	for i, ci := range clonedInfo.Codecs {
		for _, originCi := range trackInfo.Codecs {
			if !mime.IsMimeTypeStringEqual(ci.MimeType, originCi.MimeType) {
				continue
			}

			if originCi.Mid != "" {
				ci.Mid = originCi.Mid
			}

			for _, layer := range ci.Layers {
				for _, originLayer := range originCi.Layers {
					if layer.Quality == originLayer.Quality {
						if originLayer.Ssrc != 0 {
							layer.Ssrc = originLayer.Ssrc
						}
						break
					}
				}
			}
			break
		}

		// for clients that don't use simulcast codecs (old client version or single codec)
		if i == 0 {
			clonedInfo.Layers = ci.Layers
		}
	}
	if trackInfo.Muted != clonedInfo.Muted {
		updateMute = true
	}
	t.trackInfo.Store(clonedInfo)
	t.lock.Unlock()

	if updateMute {
		t.SetMuted(clonedInfo.Muted)
	}

	t.updateTrackInfoOfReceivers()
}

func (t *MediaTrackReceiver) UpdateAudioTrack(update *livekit.UpdateLocalAudioTrack) {
	if t.Kind() != livekit.TrackType_AUDIO {
		return
	}

	t.lock.Lock()
	trackInfo := t.TrackInfo()
	clonedInfo := utils.CloneProto(trackInfo)

	clonedInfo.AudioFeatures = sutils.DedupeSlice(update.Features)

	clonedInfo.Stereo = false
	clonedInfo.DisableDtx = false
	for _, feature := range update.Features {
		switch feature {
		case livekit.AudioTrackFeature_TF_STEREO:
			clonedInfo.Stereo = true
		case livekit.AudioTrackFeature_TF_NO_DTX:
			clonedInfo.DisableDtx = true
		}
	}

	if proto.Equal(trackInfo, clonedInfo) {
		t.lock.Unlock()
		return
	}

	t.trackInfo.Store(clonedInfo)
	t.lock.Unlock()

	t.updateTrackInfoOfReceivers()

	t.params.Telemetry.TrackPublishedUpdate(context.Background(), t.PublisherID(), clonedInfo)
	t.params.Logger.Debugw("updated audio track", "before", logger.Proto(trackInfo), "after", logger.Proto(clonedInfo))
}

func (t *MediaTrackReceiver) UpdateVideoTrack(update *livekit.UpdateLocalVideoTrack) {
	if t.Kind() != livekit.TrackType_VIDEO {
		return
	}

	t.lock.Lock()
	trackInfo := t.TrackInfo()
	clonedInfo := utils.CloneProto(trackInfo)
	clonedInfo.Width = update.Width
	clonedInfo.Height = update.Height
	if proto.Equal(trackInfo, clonedInfo) {
		t.lock.Unlock()
		return
	}

	t.trackInfo.Store(clonedInfo)
	t.lock.Unlock()

	t.updateTrackInfoOfReceivers()

	t.params.Telemetry.TrackPublishedUpdate(context.Background(), t.PublisherID(), clonedInfo)
	t.params.Logger.Debugw("updated video track", "before", logger.Proto(trackInfo), "after", logger.Proto(clonedInfo))
}

func (t *MediaTrackReceiver) TrackInfo() *livekit.TrackInfo {
	return t.trackInfo.Load()
}

func (t *MediaTrackReceiver) TrackInfoClone() *livekit.TrackInfo {
	return utils.CloneProto(t.TrackInfo())
}

func (t *MediaTrackReceiver) NotifyMaxLayerChange(mimeType mime.MimeType, maxLayer int32) {
	trackInfo := t.TrackInfo()
	quality := buffer.GetVideoQualityForSpatialLayer(mimeType, maxLayer, trackInfo)
	ti := &livekit.TrackInfo{
		Sid:    trackInfo.Sid,
		Type:   trackInfo.Type,
		Layers: []*livekit.VideoLayer{{Quality: quality}},
	}
	if quality != livekit.VideoQuality_OFF {
		layers := buffer.GetVideoLayersForMimeType(mimeType, trackInfo)
		for _, layer := range layers {
			if layer.Quality == quality {
				ti.Layers[0].Width = layer.Width
				ti.Layers[0].Height = layer.Height
				break
			}
		}
	}

	t.params.Telemetry.TrackPublishedUpdate(context.Background(), t.PublisherID(), ti)
}

// GetQualityForDimension finds the closest quality to use for desired dimensions
// affords a 20% tolerance on dimension
func (t *MediaTrackReceiver) GetQualityForDimension(mimeType mime.MimeType, width, height uint32) livekit.VideoQuality {
	quality := livekit.VideoQuality_HIGH
	if t.Kind() == livekit.TrackType_AUDIO {
		return quality
	}

	trackInfo := t.TrackInfo()

	var mediaSizes []buffer.VideoSize
	if receiver := t.Receiver(mimeType); receiver != nil {
		mediaSizes = receiver.VideoSizes()
	}

	if trackInfo.Height == 0 && len(mediaSizes) == 0 {
		return quality
	}
	origSize := trackInfo.Height
	requestedSize := height
	if trackInfo.Width < trackInfo.Height {
		// for portrait videos
		origSize = trackInfo.Width
		requestedSize = width
	}

	if origSize == 0 {
		for i := len(mediaSizes) - 1; i >= 0; i-- {
			if mediaSizes[i].Height > 0 {
				origSize = mediaSizes[i].Height
				if mediaSizes[i].Width < mediaSizes[i].Height {
					origSize = mediaSizes[i].Width
				}
				break
			}
		}
	}

	// default sizes representing qualities low - high
	layerSizes := []uint32{180, 360, origSize}
	var providedSizes []uint32
	for _, layer := range buffer.GetVideoLayersForMimeType(mimeType, trackInfo) {
		providedSizes = append(providedSizes, layer.Height)
	}

	if len(providedSizes) == 0 || providedSizes[0] == 0 || t.params.PreferVideoSizeFromMedia {
		if len(mediaSizes) > 0 {
			providedSizes = providedSizes[:0]
			for _, size := range mediaSizes {
				providedSizes = append(providedSizes, size.Height)
			}
		} else {
			t.params.Logger.Debugw("no video sizes provided by receiver, using track info sizes")
		}
	}

	if len(providedSizes) > 0 {
		layerSizes = providedSizes
		// comparing height always
		requestedSize = height
		sort.Slice(layerSizes, func(i, j int) bool {
			return layerSizes[i] < layerSizes[j]
		})
	}

	// finds the highest layer with smallest dimensions that still satisfy client demands
	requestedSize = uint32(float32(requestedSize) * layerSelectionTolerance)
	for i, s := range layerSizes {
		quality = livekit.VideoQuality(i)
		if i == len(layerSizes)-1 {
			break
		} else if s >= requestedSize && s != layerSizes[i+1] {
			break
		}
	}

	return quality
}

func (t *MediaTrackReceiver) GetAudioLevel() (float64, bool) {
	receiver := t.ActiveReceiver()
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
		"PubMuted": t.IsMuted(),
	}

	info["DownTracks"] = t.MediaTrackSubscriptions.DebugInfo()

	for _, receiver := range t.loadReceivers() {
		info[receiver.Codec().MimeType] = receiver.DebugInfo()
	}

	return info
}

func (t *MediaTrackReceiver) PrimaryReceiver() sfu.TrackReceiver {
	receivers := t.loadReceivers()
	if len(receivers) == 0 {
		return nil
	}
	if dr, ok := receivers[0].TrackReceiver.(*DummyReceiver); ok {
		return dr.Receiver()
	}
	return receivers[0].TrackReceiver
}

func (t *MediaTrackReceiver) ActiveReceiver() sfu.TrackReceiver {
	for _, r := range t.loadReceivers() {
		if r.IsRegressed() {
			return r.TrackReceiver
		}
	}

	return t.PrimaryReceiver()
}

func (t *MediaTrackReceiver) Receiver(mime mime.MimeType) sfu.TrackReceiver {
	for _, r := range t.loadReceivers() {
		if r.Mime() == mime {
			if dr, ok := r.TrackReceiver.(*DummyReceiver); ok {
				return dr.Receiver()
			}
			return r.TrackReceiver
		}
	}
	return nil
}

func (t *MediaTrackReceiver) Receivers() []sfu.TrackReceiver {
	receivers := t.loadReceivers()
	trackReceivers := make([]sfu.TrackReceiver, len(receivers))
	for i, r := range receivers {
		trackReceivers[i] = r.TrackReceiver
	}
	return trackReceivers
}

func (t *MediaTrackReceiver) loadReceivers() []*simulcastReceiver {
	t.lock.RLock()
	defer t.lock.RUnlock()
	return t.receivers
}

func (t *MediaTrackReceiver) SetRTT(rtt uint32) {
	for _, r := range t.loadReceivers() {
		if wr, ok := r.TrackReceiver.(*sfu.WebRTCReceiver); ok {
			wr.SetRTT(rtt)
		}
	}
}

func (t *MediaTrackReceiver) GetTemporalLayerForSpatialFps(mimeType mime.MimeType, spatial int32, fps uint32) int32 {
	receiver := t.Receiver(mimeType)
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

func (t *MediaTrackReceiver) GetTrackStats() *livekit.RTPStats {
	receivers := t.loadReceivers()
	stats := make([]*livekit.RTPStats, 0, len(receivers))
	for _, receiver := range receivers {
		receiverStats := receiver.GetTrackStats()
		if receiverStats != nil {
			stats = append(stats, receiverStats)
		}
	}

	return rtpstats.AggregateRTPStats(stats)
}
