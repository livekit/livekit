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
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"

	"github.com/livekit/mediatransportutil/pkg/codec"
	"github.com/livekit/protocol/codecs/mime"
	"github.com/livekit/protocol/livekit"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/sfu/bwe"
)

//go:generate go run github.com/maxbrunsfeld/counterfeiter/v6 -generate

// TrackReceiver defines an interface receive media from remote peer
//
//counterfeiter:generate . TrackReceiver
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
	VideoSizes() []codec.VideoSize

	// closes all associated buffers and issues a resync to all attached downtracks so that
	// they can resync and have proper sequncing without gaps in sequence numbers / timestamps
	Restart(reason string)
}

// --------------------------------------

//counterfeiter:generate . REDTransformer
type REDTransformer interface {
	TrackReceiver
	SetLBThreshold(lbThreshold int)
	ForwardRTP(pkt *buffer.ExtPacket, spatialLayer int32) int32
	ForwardRTCPSenderReport(
		payloadType webrtc.PayloadType,
		layer int32,
		publisherSRData *livekit.RTCPSenderReportState,
	)
	GetDownTracks() []TrackSender
	ResyncDownTracks()
	OnStreamRestart()
	CanClose() bool
	Close()
}

// --------------------------------------

// TrackSender defines an interface send media to remote peer
//
//counterfeiter:generate . TrackSender
type TrackSender interface {
	UpTrackLayersChange()
	UpTrackBitrateAvailabilityChange()
	UpTrackMaxPublishedLayerChange(maxPublishedLayer int32)
	UpTrackMaxTemporalLayerSeenChange(maxTemporalLayerSeen int32)
	UpTrackBitrateReport(availableLayers []int32, bitrates Bitrates)
	WriteRTP(p *buffer.ExtPacket, layer int32) int32
	Close()
	IsClosed() bool
	// ID is the globally unique identifier for this Track.
	ID() string
	SubscriberID() livekit.ParticipantID
	HandleRTCPSenderReportData(
		payloadType webrtc.PayloadType,
		layer int32,
		publisherSRData *livekit.RTCPSenderReportState,
	) error
	Resync()
	SetReceiver(TrackReceiver)
	ReceiverRestart(TrackReceiver)
}

// -------------------------------------------------------------------

//counterfeiter:generate . DownTrackStreamAllocatorListener
type DownTrackStreamAllocatorListener interface {
	// RTCP received
	OnREMB(dt *DownTrack, remb *rtcp.ReceiverEstimatedMaximumBitrate)
	OnTransportCCFeedback(dt *DownTrack, cc *rtcp.TransportLayerCC)

	// video layer availability changed
	OnAvailableLayersChanged(dt *DownTrack)

	// video layer bitrate availability changed
	OnBitrateAvailabilityChanged(dt *DownTrack)

	// max published spatial layer changed
	OnMaxPublishedSpatialChanged(dt *DownTrack)

	// max published temporal layer changed
	OnMaxPublishedTemporalChanged(dt *DownTrack)

	// subscription changed - mute/unmute
	OnSubscriptionChanged(dt *DownTrack)

	// subscribed max video layer changed
	OnSubscribedLayerChanged(dt *DownTrack, layers buffer.VideoLayer)

	// stream resumed
	OnResume(dt *DownTrack)

	// check if track should participate in BWE
	IsBWEEnabled(dt *DownTrack) bool

	// get the BWE type in use
	BWEType() bwe.BWEType

	// check if subscription mute can be applied
	IsSubscribeMutable(dt *DownTrack) bool
}

// -------------------------------------------------------------------

//counterfeiter:generate . DownTrackListener
type DownTrackListener interface {
	OnBindAndConnected()
	OnStatsUpdate(stat *livekit.AnalyticsStat)
	OnMaxSubscribedLayerChanged(layer int32)
	OnRttUpdate(rtt uint32)
	OnCodecNegotiated(webrtc.RTPCodecCapability)
	OnDownTrackClose(isExpectedToResume bool)
	OnStreamStarted()
}

// -------------------------------------------------------------------
