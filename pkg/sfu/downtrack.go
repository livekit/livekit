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
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/sdp/v3"
	"github.com/pion/transport/v2/packetio"
	"github.com/pion/webrtc/v3"
	"go.uber.org/atomic"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/sfu/connectionquality"
	"github.com/livekit/livekit-server/pkg/sfu/pacer"
	act "github.com/livekit/livekit-server/pkg/sfu/rtpextension/abscapturetime"
	dd "github.com/livekit/livekit-server/pkg/sfu/rtpextension/dependencydescriptor"
	pd "github.com/livekit/livekit-server/pkg/sfu/rtpextension/playoutdelay"
	"github.com/livekit/livekit-server/pkg/sfu/utils"
)

// TrackSender defines an interface send media to remote peer
type TrackSender interface {
	UpTrackLayersChange()
	UpTrackBitrateAvailabilityChange()
	UpTrackMaxPublishedLayerChange(maxPublishedLayer int32)
	UpTrackMaxTemporalLayerSeenChange(maxTemporalLayerSeen int32)
	UpTrackBitrateReport(availableLayers []int32, bitrates Bitrates)
	WriteRTP(p *buffer.ExtPacket, layer int32) error
	Close()
	IsClosed() bool
	// ID is the globally unique identifier for this Track.
	ID() string
	SubscriberID() livekit.ParticipantID
	TrackInfoAvailable()
	HandleRTCPSenderReportData(
		payloadType webrtc.PayloadType,
		isSVC bool,
		layer int32,
		publisherSRData *buffer.RTCPSenderReportData,
	) error
	Resync()
}

// -------------------------------------------------------------------

const (
	RTPPaddingMaxPayloadSize      = 255
	RTPPaddingEstimatedHeaderSize = 20
	RTPBlankFramesMuteSeconds     = float32(1.0)
	RTPBlankFramesCloseSeconds    = float32(0.2)

	FlagStopRTXOnPLI = true

	keyFrameIntervalMin = 200
	keyFrameIntervalMax = 1000
	flushTimeout        = 1 * time.Second

	waitBeforeSendPaddingOnMute = 100 * time.Millisecond
	maxPaddingOnMuteDuration    = 5 * time.Second
)

// -------------------------------------------------------------------

var (
	ErrUnknownKind                       = errors.New("unknown kind of codec")
	ErrOutOfOrderSequenceNumberCacheMiss = errors.New("out-of-order sequence number not found in cache")
	ErrPaddingOnlyPacket                 = errors.New("padding only packet that need not be forwarded")
	ErrDuplicatePacket                   = errors.New("duplicate packet")
	ErrPaddingNotOnFrameBoundary         = errors.New("padding cannot send on non-frame boundary")
	ErrDownTrackAlreadyBound             = errors.New("already bound")
	ErrPayloadOverflow                   = errors.New("payload overflow")
)

var (
	VP8KeyFrame8x8 = []byte{
		0x10, 0x02, 0x00, 0x9d, 0x01, 0x2a, 0x08, 0x00,
		0x08, 0x00, 0x00, 0x47, 0x08, 0x85, 0x85, 0x88,
		0x85, 0x84, 0x88, 0x02, 0x02, 0x00, 0x0c, 0x0d,
		0x60, 0x00, 0xfe, 0xff, 0xab, 0x50, 0x80,
	}

	H264KeyFrame2x2SPS = []byte{
		0x67, 0x42, 0xc0, 0x1f, 0x0f, 0xd9, 0x1f, 0x88,
		0x88, 0x84, 0x00, 0x00, 0x03, 0x00, 0x04, 0x00,
		0x00, 0x03, 0x00, 0xc8, 0x3c, 0x60, 0xc9, 0x20,
	}
	H264KeyFrame2x2PPS = []byte{
		0x68, 0x87, 0xcb, 0x83, 0xcb, 0x20,
	}
	H264KeyFrame2x2IDR = []byte{
		0x65, 0x88, 0x84, 0x0a, 0xf2, 0x62, 0x80, 0x00,
		0xa7, 0xbe,
	}
	H264KeyFrame2x2 = [][]byte{H264KeyFrame2x2SPS, H264KeyFrame2x2PPS, H264KeyFrame2x2IDR}

	OpusSilenceFrame = []byte{
		0xf8, 0xff, 0xfe, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	}
)

// -------------------------------------------------------------------

type DownTrackState struct {
	RTPStats                   *buffer.RTPStatsSender
	DeltaStatsSenderSnapshotId uint32
	ForwarderState             ForwarderState
}

func (d DownTrackState) String() string {
	return fmt.Sprintf("DownTrackState{rtpStats: %s, deltaSender: %d, forwarder: %s}",
		d.RTPStats, d.DeltaStatsSenderSnapshotId, d.ForwarderState.String())
}

// -------------------------------------------------------------------

/* STREAM-ALLOCATOR-DATA
type NackInfo struct {
	Timestamp      uint32
	SequenceNumber uint16
	Attempts       uint8
}
*/

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

	// packet(s) sent
	OnPacketsSent(dt *DownTrack, size int)

	/* STREAM-ALLOCATOR-DATA
	// NACKs received
	OnNACK(dt *DownTrack, nackInfos []NackInfo)

	// RTCP Receiver Report received
	OnRTCPReceiverReport(dt *DownTrack, rr rtcp.ReceptionReport)
	*/

	// check if track should participate in BWE
	IsBWEEnabled(dt *DownTrack) bool

	// check if subscription mute can be applied
	IsSubscribeMutable(dt *DownTrack) bool
}

type ReceiverReportListener func(dt *DownTrack, report *rtcp.ReceiverReport)

type DowntrackParams struct {
	Codecs            []webrtc.RTPCodecParameters
	Source            livekit.TrackSource
	Receiver          TrackReceiver
	BufferFactory     *buffer.Factory
	SubID             livekit.ParticipantID
	StreamID          string
	MaxTrack          int
	PlayoutDelayLimit *livekit.PlayoutDelay
	Pacer             pacer.Pacer
	Logger            logger.Logger
	Trailer           []byte
	RTCPWriter        func([]rtcp.Packet) error
}

// DownTrack implements TrackLocal, is the track used to write packets
// to SFU Subscriber, the track handle the packets for simple, simulcast
// and SVC Publisher.
// A DownTrack has the following lifecycle
// - new
// - bound / unbound
// - closed
// once closed, a DownTrack cannot be re-used.
type DownTrack struct {
	params      DowntrackParams
	id          livekit.TrackID
	kind        webrtc.RTPCodecType
	mime        string
	ssrc        uint32
	payloadType uint8
	sequencer   *sequencer

	forwarder *Forwarder

	upstreamCodecs            []webrtc.RTPCodecParameters
	codec                     webrtc.RTPCodecCapability
	absSendTimeExtID          int
	transportWideExtID        int
	dependencyDescriptorExtID int
	playoutDelayExtID         int
	absCaptureTimeExtID       int
	transceiver               atomic.Pointer[webrtc.RTPTransceiver]
	writeStream               webrtc.TrackLocalWriter
	rtcpReader                *buffer.RTCPReader

	listenerLock            sync.RWMutex
	receiverReportListeners []ReceiverReportListener

	bindLock  sync.Mutex
	bound     atomic.Bool
	onBinding func(error)

	isClosed             atomic.Bool
	connected            atomic.Bool
	bindAndConnectedOnce atomic.Bool
	writable             atomic.Bool

	rtpStats *buffer.RTPStatsSender

	totalRepeatedNACKs atomic.Uint32

	blankFramesGeneration atomic.Uint32

	connectionStats            *connectionquality.ConnectionStats
	deltaStatsSenderSnapshotId uint32

	isNACKThrottled atomic.Bool

	activePaddingOnMuteUpTrack atomic.Bool

	streamAllocatorLock             sync.RWMutex
	streamAllocatorListener         DownTrackStreamAllocatorListener
	streamAllocatorReportGeneration int
	streamAllocatorBytesCounter     atomic.Uint32
	/* STREAM-ALLOCATOR-DATA
	bytesSent                       atomic.Uint32
	bytesRetransmitted              atomic.Uint32
	*/

	playoutDelay *PlayoutDelayController

	pacer pacer.Pacer

	maxLayerNotifierChMu     sync.RWMutex
	maxLayerNotifierCh       chan string
	maxLayerNotifierChClosed bool

	keyFrameRequesterChMu     sync.RWMutex
	keyFrameRequesterCh       chan struct{}
	keyFrameRequesterChClosed bool

	cbMu                        sync.RWMutex
	onStatsUpdate               func(dt *DownTrack, stat *livekit.AnalyticsStat)
	onMaxSubscribedLayerChanged func(dt *DownTrack, layer int32)
	onRttUpdate                 func(dt *DownTrack, rtt uint32)
	onCloseHandler              func(isExpectedToResume bool)

	createdAt int64
}

// NewDownTrack returns a DownTrack.
func NewDownTrack(params DowntrackParams) (*DownTrack, error) {
	codecs := params.Codecs
	var kind webrtc.RTPCodecType
	switch {
	case strings.HasPrefix(codecs[0].MimeType, "audio/"):
		kind = webrtc.RTPCodecTypeAudio
	case strings.HasPrefix(codecs[0].MimeType, "video/"):
		kind = webrtc.RTPCodecTypeVideo
	default:
		kind = webrtc.RTPCodecType(0)
	}

	d := &DownTrack{
		params:              params,
		id:                  params.Receiver.TrackID(),
		upstreamCodecs:      codecs,
		kind:                kind,
		codec:               codecs[0].RTPCodecCapability,
		pacer:               params.Pacer,
		maxLayerNotifierCh:  make(chan string, 1),
		keyFrameRequesterCh: make(chan struct{}, 1),
		createdAt:           time.Now().UnixNano(),
	}
	d.params.Logger = params.Logger.WithValues(
		"mime", codecs[0].MimeType,
		"subscriberID", d.SubscriberID(),
	)
	d.forwarder = NewForwarder(
		d.kind,
		d.params.Logger,
		false,
		d.getExpectedRTPTimestamp,
	)

	d.rtpStats = buffer.NewRTPStatsSender(buffer.RTPStatsParams{
		ClockRate: d.codec.ClockRate,
		Logger:    d.params.Logger,
	})
	d.deltaStatsSenderSnapshotId = d.rtpStats.NewSenderSnapshotId()

	d.connectionStats = connectionquality.NewConnectionStats(connectionquality.ConnectionStatsParams{
		MimeType:       codecs[0].MimeType, // LK-TODO have to notify on codec change
		IsFECEnabled:   strings.EqualFold(codecs[0].MimeType, webrtc.MimeTypeOpus) && strings.Contains(strings.ToLower(codecs[0].SDPFmtpLine), "fec"),
		SenderProvider: d,
		Logger:         d.params.Logger.WithValues("direction", "down"),
	})
	d.connectionStats.OnStatsUpdate(func(_cs *connectionquality.ConnectionStats, stat *livekit.AnalyticsStat) {
		if onStatsUpdate := d.getOnStatsUpdate(); onStatsUpdate != nil {
			onStatsUpdate(d, stat)
		}
	})

	if d.kind == webrtc.RTPCodecTypeVideo {
		if delay := params.PlayoutDelayLimit; delay.GetEnabled() {
			var err error
			d.playoutDelay, err = NewPlayoutDelayController(delay.GetMin(), delay.GetMax(), params.Logger, d.rtpStats)
			if err != nil {
				return nil, err
			}
		}
		go d.maxLayerNotifierWorker()
		go d.keyFrameRequester()
	}
	d.params.Logger.Debugw("downtrack created")

	return d, nil
}

// Bind is called by the PeerConnection after negotiation is complete
// This asserts that the code requested is supported by the remote peer.
// If so it sets up all the state (SSRC and PayloadType) to have a call
func (d *DownTrack) Bind(t webrtc.TrackLocalContext) (webrtc.RTPCodecParameters, error) {
	d.bindLock.Lock()
	if d.bound.Load() {
		d.bindLock.Unlock()
		return webrtc.RTPCodecParameters{}, ErrDownTrackAlreadyBound
	}
	var codec webrtc.RTPCodecParameters
	for _, c := range d.upstreamCodecs {
		matchCodec, err := utils.CodecParametersFuzzySearch(c, t.CodecParameters())
		if err == nil {
			codec = matchCodec
			break
		}
	}

	if codec.MimeType == "" {
		err := webrtc.ErrUnsupportedCodec
		onBinding := d.onBinding
		d.bindLock.Unlock()
		d.params.Logger.Infow("bind error for unsupported codec", "codecs", d.upstreamCodecs, "remoteParameters", t.CodecParameters())
		if onBinding != nil {
			onBinding(err)
		}
		return webrtc.RTPCodecParameters{}, err
	}

	// if a downtrack is closed before bind, it already unsubscribed from client, don't do subsequent operation and return here.
	if d.IsClosed() {
		d.params.Logger.Debugw("DownTrack closed before bind")
		d.bindLock.Unlock()
		return codec, nil
	}

	d.params.Logger.Debugw("DownTrack.Bind", "codecs", d.upstreamCodecs, "matchCodec", codec, "ssrc", t.SSRC())
	d.ssrc = uint32(t.SSRC())
	d.payloadType = uint8(codec.PayloadType)
	d.writeStream = t.WriteStream()
	d.mime = strings.ToLower(codec.MimeType)
	if rr := d.params.BufferFactory.GetOrNew(packetio.RTCPBufferPacket, uint32(t.SSRC())).(*buffer.RTCPReader); rr != nil {
		rr.OnPacket(func(pkt []byte) {
			d.handleRTCP(pkt)
		})
		d.rtcpReader = rr
	}

	d.sequencer = newSequencer(d.params.MaxTrack, d.kind == webrtc.RTPCodecTypeVideo, d.params.Logger)

	d.codec = codec.RTPCodecCapability
	if d.onBinding != nil {
		d.onBinding(nil)
	}
	d.bound.Store(true)
	d.onBindAndConnectedChange()
	d.bindLock.Unlock()

	// Bind is called under RTPSender.mu lock, call the RTPSender.GetParameters in goroutine to avoid deadlock
	go func() {
		if tr := d.transceiver.Load(); tr != nil {
			if sender := tr.Sender(); sender != nil {
				extensions := sender.GetParameters().HeaderExtensions
				d.params.Logger.Debugw("negotiated downtrack extensions", "extensions", extensions)
				d.SetRTPHeaderExtensions(extensions)
			}
		}
	}()

	d.forwarder.DetermineCodec(d.codec, d.params.Receiver.HeaderExtensions())
	d.params.Logger.Debugw("downtrack bound")

	return codec, nil
}

// Unbind implements the teardown logic when the track is no longer needed. This happens
// because a track has been stopped.
func (d *DownTrack) Unbind(_ webrtc.TrackLocalContext) error {
	d.bindLock.Lock()
	d.bound.Store(false)
	d.onBindAndConnectedChange()
	d.bindLock.Unlock()
	return nil
}

func (d *DownTrack) TrackInfoAvailable() {
	ti := d.params.Receiver.TrackInfo()
	if ti == nil {
		return
	}
	d.connectionStats.Start(ti)
}

func (d *DownTrack) SetStreamAllocatorListener(listener DownTrackStreamAllocatorListener) {
	d.streamAllocatorLock.Lock()
	d.streamAllocatorListener = listener
	d.streamAllocatorLock.Unlock()

	if listener != nil {
		if !listener.IsBWEEnabled(d) {
			d.absSendTimeExtID = 0
			d.transportWideExtID = 0
		}

		// kick off a gratuitous allocation
		listener.OnSubscriptionChanged(d)
	}
}

func (d *DownTrack) getStreamAllocatorListener() DownTrackStreamAllocatorListener {
	d.streamAllocatorLock.RLock()
	defer d.streamAllocatorLock.RUnlock()

	return d.streamAllocatorListener
}

func (d *DownTrack) SetStreamAllocatorReportInterval(interval time.Duration) {
	d.ClearStreamAllocatorReportInterval()

	if interval == 0 {
		return
	}

	d.streamAllocatorLock.Lock()
	d.streamAllocatorBytesCounter.Store(0)

	d.streamAllocatorReportGeneration++
	gen := d.streamAllocatorReportGeneration
	d.streamAllocatorLock.Unlock()

	go func(generation int) {
		timer := time.NewTimer(interval)
		for {
			<-timer.C

			d.streamAllocatorLock.Lock()
			if generation != d.streamAllocatorReportGeneration {
				d.streamAllocatorLock.Unlock()
				return
			}

			sal := d.streamAllocatorListener
			bytes := d.streamAllocatorBytesCounter.Swap(0)
			d.streamAllocatorLock.Unlock()

			if sal != nil {
				sal.OnPacketsSent(d, int(bytes))
			}

			timer.Reset(interval)
		}
	}(gen)
}

func (d *DownTrack) ClearStreamAllocatorReportInterval() {
	d.streamAllocatorLock.Lock()
	d.streamAllocatorReportGeneration++
	d.streamAllocatorLock.Unlock()
}

// ID is the unique identifier for this Track. This should be unique for the
// stream, but doesn't have to globally unique. A common example would be 'audio' or 'video'
// and StreamID would be 'desktop' or 'webcam'
func (d *DownTrack) ID() string { return string(d.id) }

// Codec returns current track codec capability
func (d *DownTrack) Codec() webrtc.RTPCodecCapability { return d.codec }

// StreamID is the group this track belongs too. This must be unique
func (d *DownTrack) StreamID() string { return d.params.StreamID }

func (d *DownTrack) SubscriberID() livekit.ParticipantID {
	// add `createdAt` to ensure repeated subscriptions from same subscrober to same publisher does not collide
	return livekit.ParticipantID(fmt.Sprintf("%s:%d", d.params.SubID, d.createdAt))
}

// Sets RTP header extensions for this track
func (d *DownTrack) SetRTPHeaderExtensions(rtpHeaderExtensions []webrtc.RTPHeaderExtensionParameter) {
	isBWEEnabled := true
	if sal := d.getStreamAllocatorListener(); sal != nil {
		isBWEEnabled = sal.IsBWEEnabled(d)
	}
	for _, ext := range rtpHeaderExtensions {
		switch ext.URI {
		case sdp.ABSSendTimeURI:
			if isBWEEnabled {
				d.absSendTimeExtID = ext.ID
			} else {
				d.absSendTimeExtID = 0
			}
		case dd.ExtensionURI:
			d.dependencyDescriptorExtID = ext.ID
		case pd.PlayoutDelayURI:
			d.playoutDelayExtID = ext.ID
		case sdp.TransportCCURI:
			if isBWEEnabled {
				d.transportWideExtID = ext.ID
			} else {
				d.transportWideExtID = 0
			}
		case act.AbsCaptureTimeURI:
			d.absCaptureTimeExtID = ext.ID
		}
	}
}

// Kind controls if this TrackLocal is audio or video
func (d *DownTrack) Kind() webrtc.RTPCodecType {
	return d.kind
}

// RID is required by `webrtc.TrackLocal` interface
func (d *DownTrack) RID() string {
	return ""
}

func (d *DownTrack) SSRC() uint32 {
	return d.ssrc
}

func (d *DownTrack) Stop() error {
	if tr := d.transceiver.Load(); tr != nil {
		return tr.Stop()
	}
	return errors.New("downtrack transceiver does not exist")
}

func (d *DownTrack) SetTransceiver(transceiver *webrtc.RTPTransceiver) {
	d.transceiver.Store(transceiver)
}

func (d *DownTrack) GetTransceiver() *webrtc.RTPTransceiver {
	return d.transceiver.Load()
}

func (d *DownTrack) postKeyFrameRequestEvent() {
	if d.kind != webrtc.RTPCodecTypeVideo {
		return
	}

	d.keyFrameRequesterChMu.RLock()
	if !d.keyFrameRequesterChClosed {
		select {
		case d.keyFrameRequesterCh <- struct{}{}:
		default:
		}
	}
	d.keyFrameRequesterChMu.RUnlock()
}

func (d *DownTrack) keyFrameRequester() {
	getInterval := func() time.Duration {
		interval := 2 * d.rtpStats.GetRtt()
		if interval < keyFrameIntervalMin {
			interval = keyFrameIntervalMin
		}
		if interval > keyFrameIntervalMax {
			interval = keyFrameIntervalMax
		}
		return time.Duration(interval) * time.Millisecond
	}

	timer := time.NewTimer(math.MaxInt64)
	timer.Stop()

	defer timer.Stop()

	for !d.IsClosed() {
		timer.Reset(getInterval())

		select {
		case _, more := <-d.keyFrameRequesterCh:
			if !more {
				return
			}
			if !timer.Stop() {
				<-timer.C
			}
		case <-timer.C:
		}

		locked, layer := d.forwarder.CheckSync()
		if !locked && layer != buffer.InvalidLayerSpatial && d.writable.Load() {
			d.params.Logger.Debugw("sending PLI for layer lock", "layer", layer)
			d.params.Receiver.SendPLI(layer, false)
			d.rtpStats.UpdateLayerLockPliAndTime(1)
		}
	}
}

func (d *DownTrack) postMaxLayerNotifierEvent(event string) {
	if d.kind != webrtc.RTPCodecTypeVideo {
		return
	}

	d.maxLayerNotifierChMu.RLock()
	if !d.maxLayerNotifierChClosed {
		select {
		case d.maxLayerNotifierCh <- event:
		default:
			d.params.Logger.Debugw("max layer notifier channel busy", "event", event)
		}
	}
	d.maxLayerNotifierChMu.RUnlock()
}

func (d *DownTrack) maxLayerNotifierWorker() {
	for event := range d.maxLayerNotifierCh {
		maxLayerSpatial := d.forwarder.GetMaxSubscribedSpatial()
		d.params.Logger.Debugw("max subscribed layer processed", "layer", maxLayerSpatial, "event", event)

		if onMaxSubscribedLayerChanged := d.getOnMaxLayerChanged(); onMaxSubscribedLayerChanged != nil {
			d.params.Logger.Debugw(
				"notifying max subscribed layer",
				"layer", maxLayerSpatial,
				"event", event,
			)
			onMaxSubscribedLayerChanged(d, maxLayerSpatial)
		}
	}

	if onMaxSubscribedLayerChanged := d.getOnMaxLayerChanged(); onMaxSubscribedLayerChanged != nil {
		d.params.Logger.Debugw(
			"notifying max subscribed layer",
			"layer", buffer.InvalidLayerSpatial,
			"event", "close",
		)
		onMaxSubscribedLayerChanged(d, buffer.InvalidLayerSpatial)
	}
}

// WriteRTP writes an RTP Packet to the DownTrack
func (d *DownTrack) WriteRTP(extPkt *buffer.ExtPacket, layer int32) error {
	if !d.writable.Load() {
		return nil
	}

	tp, err := d.forwarder.GetTranslationParams(extPkt, layer)
	if tp.shouldDrop {
		if err != nil {
			d.params.Logger.Errorw("could not get translation params", err)
		}
		return err
	}

	poolEntity := PacketFactory.Get().(*[]byte)
	payload := *poolEntity
	copy(payload, tp.codecBytes)
	n := copy(payload[len(tp.codecBytes):], extPkt.Packet.Payload[tp.incomingHeaderSize:])
	if n != len(extPkt.Packet.Payload[tp.incomingHeaderSize:]) {
		d.params.Logger.Errorw("payload overflow", nil, "want", len(extPkt.Packet.Payload[tp.incomingHeaderSize:]), "have", n)
		PacketFactory.Put(poolEntity)
		return ErrPayloadOverflow
	}
	payload = payload[:len(tp.codecBytes)+n]

	hdr, err := d.getTranslatedRTPHeader(extPkt, &tp)
	if err != nil {
		d.params.Logger.Errorw("could not get translated RTP header", err)
		PacketFactory.Put(poolEntity)
		return err
	}

	var extensions []pacer.ExtensionData
	if tp.ddBytes != nil {
		extensions = append(
			extensions,
			pacer.ExtensionData{
				ID:      uint8(d.dependencyDescriptorExtID),
				Payload: tp.ddBytes,
			},
		)
	}
	if d.playoutDelayExtID != 0 && d.playoutDelay != nil {
		if val := d.playoutDelay.GetDelayExtension(hdr.SequenceNumber); val != nil {
			extensions = append(
				extensions,
				pacer.ExtensionData{
					ID:      uint8(d.playoutDelayExtID),
					Payload: val,
				},
			)

			// NOTE: play out delay extension is not cached in sequencer,
			// i. e. they will not be added to retransmitted packet.
			// But, it is okay as the extension is added till a RTCP Receiver Report for
			// the corresponding sequence number is received.
			// The extreme case is all packets containing the play out delay are lost and
			// all of them retransmitted and an RTCP Receiver Report received for those
			// retransmited sequence numbers. But, that is highly improbable, if not impossible.
		}
	}
	var actBytes []byte
	if extPkt.AbsCaptureTimeExt != nil && d.absCaptureTimeExtID != 0 {
		// normalize capture time to SFU clock.
		// NOTE: even if there is estimated offset populated, just re-map the
		// absolute capture time stamp as it should be the same RTCP sender report
		// clock domain of publisher. SFU is normalising sender reports of publisher
		// to SFU clock before sending to subscribers. So, capture time should be
		// normalized to the same clock. Clear out any offset.
		_, _, refSenderReport := d.forwarder.GetSenderReportParams()
		if refSenderReport != nil {
			actExtCopy := *extPkt.AbsCaptureTimeExt
			if err = actExtCopy.Rewrite(refSenderReport.PropagationDelay()); err == nil {
				actBytes, err = actExtCopy.Marshal()
				if err == nil {
					extensions = append(
						extensions,
						pacer.ExtensionData{
							ID:      uint8(d.absCaptureTimeExtID),
							Payload: actBytes,
						},
					)
				}
			}
		}
	}

	if d.sequencer != nil {
		d.sequencer.push(
			extPkt.Arrival,
			extPkt.ExtSequenceNumber,
			tp.rtp.extSequenceNumber,
			tp.rtp.extTimestamp,
			hdr.Marker,
			int8(layer),
			payload[:len(tp.codecBytes)],
			tp.incomingHeaderSize,
			tp.ddBytes,
			actBytes,
		)
	}

	d.sendingPacket(
		hdr,
		len(payload),
		&sendPacketMetadata{
			layer:             layer,
			packetTime:        extPkt.Arrival,
			extSequenceNumber: tp.rtp.extSequenceNumber,
			extTimestamp:      tp.rtp.extTimestamp,
			isKeyFrame:        extPkt.KeyFrame,
			tp:                &tp,
		},
	)
	d.pacer.Enqueue(pacer.Packet{
		Header:             hdr,
		Extensions:         extensions,
		Payload:            payload,
		AbsSendTimeExtID:   uint8(d.absSendTimeExtID),
		TransportWideExtID: uint8(d.transportWideExtID),
		WriteStream:        d.writeStream,
		Pool:               PacketFactory,
		PoolEntity:         poolEntity,
	})
	return nil
}

// WritePaddingRTP tries to write as many padding only RTP packets as necessary
// to satisfy given size to the DownTrack
func (d *DownTrack) WritePaddingRTP(bytesToSend int, paddingOnMute bool, forceMarker bool) int {
	if !d.writable.Load() {
		return 0
	}

	if !d.rtpStats.IsActive() && !paddingOnMute {
		return 0
	}

	// Ideally should look at header extensions negotiated for
	// track and decide if padding can be sent. But, browsers behave
	// in unexpected ways when using audio for bandwidth estimation and
	// padding is mainly used to probe for excess available bandwidth.
	// So, to be safe, limit to video tracks
	if d.kind == webrtc.RTPCodecTypeAudio {
		return 0
	}

	// LK-TODO-START
	// Potentially write padding even if muted. Given that padding
	// can be sent only on frame boundaries, writing on disabled tracks
	// will give more options.
	// LK-TODO-END
	if d.forwarder.IsMuted() && !paddingOnMute {
		return 0
	}

	// Hold sending padding packets till first RTCP-RR is received for this RTP stream.
	// That is definitive proof that the remote side knows about this RTP stream.
	if d.rtpStats.LastReceiverReportTime().IsZero() && !paddingOnMute {
		return 0
	}

	// RTP padding maximum is 255 bytes. Break it up.
	// Use 20 byte as estimate of RTP header size (12 byte header + 8 byte extension)
	num := (bytesToSend + RTPPaddingMaxPayloadSize + RTPPaddingEstimatedHeaderSize - 1) / (RTPPaddingMaxPayloadSize + RTPPaddingEstimatedHeaderSize)
	if num == 0 {
		return 0
	}

	snts, err := d.forwarder.GetSnTsForPadding(num, forceMarker)
	if err != nil {
		return 0
	}

	//
	// Register with sequencer as padding only so that NACKs for these can be filtered out.
	// Retransmission is probably a sign of network congestion/badness.
	// So, retransmitting padding only packets is only going to make matters worse.
	//
	if d.sequencer != nil {
		d.sequencer.pushPadding(snts[0].extSequenceNumber, snts[len(snts)-1].extSequenceNumber)
	}

	bytesSent := 0
	for i := 0; i < len(snts); i++ {
		hdr := rtp.Header{
			Version:        2,
			Padding:        true,
			Marker:         false,
			PayloadType:    d.payloadType,
			SequenceNumber: uint16(snts[i].extSequenceNumber),
			Timestamp:      uint32(snts[i].extTimestamp),
			SSRC:           d.ssrc,
			CSRC:           []uint32{},
		}

		payload := make([]byte, RTPPaddingMaxPayloadSize)
		// last byte of padding has padding size including that byte
		payload[RTPPaddingMaxPayloadSize-1] = byte(RTPPaddingMaxPayloadSize)

		d.sendingPacket(
			&hdr,
			len(payload),
			&sendPacketMetadata{
				packetTime:           time.Now(),
				extSequenceNumber:    snts[i].extSequenceNumber,
				extTimestamp:         snts[i].extTimestamp,
				isPadding:            true,
				shouldDisableCounter: true,
			},
		)
		d.pacer.Enqueue(pacer.Packet{
			Header:             &hdr,
			Payload:            payload,
			AbsSendTimeExtID:   uint8(d.absSendTimeExtID),
			TransportWideExtID: uint8(d.transportWideExtID),
			WriteStream:        d.writeStream,
		})

		bytesSent += hdr.MarshalSize() + len(payload)
	}

	// STREAM_ALLOCATOR-TODO: change this to pull this counter from stream allocator so that counter can be updated in pacer callback
	return bytesSent
}

// Mute enables or disables media forwarding - subscriber triggered
func (d *DownTrack) Mute(muted bool) {
	isSubscribeMutable := true
	if sal := d.getStreamAllocatorListener(); sal != nil {
		isSubscribeMutable = sal.IsSubscribeMutable(d)
	}
	changed := d.forwarder.Mute(muted, isSubscribeMutable)
	d.handleMute(muted, changed)
}

// PubMute enables or disables media forwarding - publisher side
func (d *DownTrack) PubMute(pubMuted bool) {
	changed := d.forwarder.PubMute(pubMuted)
	d.handleMute(pubMuted, changed)
}

func (d *DownTrack) handleMute(muted bool, changed bool) {
	if !changed {
		return
	}

	d.connectionStats.UpdateMute(d.forwarder.IsAnyMuted())

	//
	// Subscriber mute changes trigger a max layer notification.
	// That could result in encoding layers getting turned on/off on publisher side
	// (depending on aggregate layer requirements of all subscribers of the track).
	//
	// Publisher mute changes should not trigger notification.
	// If publisher turns off all layers because of subscribers indicating
	// no layers required due to publisher mute (bit of circular dependency),
	// there will be a delay in layers turning back on when unmute happens.
	// Unmute path will require
	//   1. unmute signalling out-of-band from publisher received by down track(s)
	//   2. down track(s) notifying max layer
	//   3. out-of-band notification about max layer sent back to the publisher
	//   4. publisher starts layer(s)
	// Ideally, on publisher mute, whatever layers were active remain active and
	// can be restarted by publisher immediately on unmute.
	//
	// Note that while publisher mute is active, subscriber changes can also happen
	// and that could turn on/off layers on publisher side.
	//
	d.postMaxLayerNotifierEvent("mute")

	if sal := d.getStreamAllocatorListener(); sal != nil {
		sal.OnSubscriptionChanged(d)
	}

	// when muting, send a few silence frames to ensure residual noise does not
	// put the comfort noise generator on decoder side in a bad state where it
	// generates noise that is not so comfortable.
	//
	// One possibility is not to inject blank frames when publisher is muted
	// and let forwarding continue. When publisher is muted, unless the media
	// stream is stopped, publisher will send silence frames which should have
	// comfort noise information. But, in case the publisher stops at an
	// inopportune frame (due to media stream stop or injecting audio from a file),
	// the decoder could be in a noisy state. So, inject blank frames on publisher
	// mute too.
	d.blankFramesGeneration.Inc()
	if d.kind == webrtc.RTPCodecTypeAudio && muted {
		d.writeBlankFrameRTP(RTPBlankFramesMuteSeconds, d.blankFramesGeneration.Load())
	}
}

func (d *DownTrack) IsClosed() bool {
	return d.isClosed.Load()
}

func (d *DownTrack) Close() {
	d.CloseWithFlush(true)
}

// CloseWithFlush - flush used to indicate whether send blank frame to flush
// decoder of client.
//  1. When transceiver is reused by other participant's video track,
//     set flush=true to avoid previous video shows before new stream is displayed.
//  2. in case of session migration, participant migrate from other node, video track should
//     be resumed with same participant, set flush=false since we don't need to flush decoder.
func (d *DownTrack) CloseWithFlush(flush bool) {
	if d.isClosed.Swap(true) {
		// already closed
		return
	}

	d.bindLock.Lock()
	d.params.Logger.Debugw("close down track", "flushBlankFrame", flush)
	if d.bound.Load() {
		d.forwarder.Mute(true, true)

		// write blank frames after disabling so that other frames do not interfere.
		// Idea here is to send blank key frames to flush the decoder buffer at the remote end.
		// Otherwise, with transceiver re-use last frame from previous stream is held in the
		// display buffer and there could be a brief moment where the previous stream is displayed.
		if flush {
			doneFlushing := d.writeBlankFrameRTP(RTPBlankFramesCloseSeconds, d.blankFramesGeneration.Inc())

			// wait a limited time to flush
			timer := time.NewTimer(flushTimeout)
			defer timer.Stop()

			select {
			case <-doneFlushing:
			case <-timer.C:
				d.blankFramesGeneration.Inc() // in case flush is still running
			}
		}

		d.bound.Store(false)
		d.onBindAndConnectedChange()
		d.params.Logger.Debugw("closing sender", "kind", d.kind)
	}
	d.params.Receiver.DeleteDownTrack(d.SubscriberID())

	if d.rtcpReader != nil && flush {
		d.params.Logger.Debugw("downtrack close rtcp reader")
		d.rtcpReader.Close()
		d.rtcpReader.OnPacket(nil)
	}
	d.bindLock.Unlock()

	d.connectionStats.Close()

	d.rtpStats.Stop()
	d.params.Logger.Debugw("rtp stats",
		"direction", "downstream",
		"mime", d.mime,
		"ssrc", d.ssrc,
		"stats", d.rtpStats,
	)

	d.maxLayerNotifierChMu.Lock()
	d.maxLayerNotifierChClosed = true
	close(d.maxLayerNotifierCh)
	d.maxLayerNotifierChMu.Unlock()

	d.keyFrameRequesterChMu.Lock()
	d.keyFrameRequesterChClosed = true
	close(d.keyFrameRequesterCh)
	d.keyFrameRequesterChMu.Unlock()

	if onCloseHandler := d.getOnCloseHandler(); onCloseHandler != nil {
		onCloseHandler(!flush)
	}

	d.ClearStreamAllocatorReportInterval()
}

func (d *DownTrack) SetMaxSpatialLayer(spatialLayer int32) {
	changed, maxLayer := d.forwarder.SetMaxSpatialLayer(spatialLayer)
	if !changed {
		return
	}

	d.postMaxLayerNotifierEvent("max-subscribed")

	if sal := d.getStreamAllocatorListener(); sal != nil {
		sal.OnSubscribedLayerChanged(d, maxLayer)
	}
}

func (d *DownTrack) SetMaxTemporalLayer(temporalLayer int32) {
	changed, maxLayer := d.forwarder.SetMaxTemporalLayer(temporalLayer)
	if !changed {
		return
	}

	if sal := d.getStreamAllocatorListener(); sal != nil {
		sal.OnSubscribedLayerChanged(d, maxLayer)
	}
}

func (d *DownTrack) MaxLayer() buffer.VideoLayer {
	return d.forwarder.MaxLayer()
}

func (d *DownTrack) GetState() DownTrackState {
	dts := DownTrackState{
		RTPStats:                   d.rtpStats,
		DeltaStatsSenderSnapshotId: d.deltaStatsSenderSnapshotId,
		ForwarderState:             d.forwarder.GetState(),
	}
	return dts
}

func (d *DownTrack) SeedState(state DownTrackState) {
	d.rtpStats.Seed(state.RTPStats)
	d.deltaStatsSenderSnapshotId = state.DeltaStatsSenderSnapshotId
	d.forwarder.SeedState(state.ForwarderState)
}

func (d *DownTrack) UpTrackLayersChange() {
	if sal := d.getStreamAllocatorListener(); sal != nil {
		sal.OnAvailableLayersChanged(d)
	}
}

func (d *DownTrack) UpTrackBitrateAvailabilityChange() {
	if sal := d.getStreamAllocatorListener(); sal != nil {
		sal.OnBitrateAvailabilityChanged(d)
	}
}

func (d *DownTrack) UpTrackMaxPublishedLayerChange(maxPublishedLayer int32) {
	if d.forwarder.SetMaxPublishedLayer(maxPublishedLayer) {
		if sal := d.getStreamAllocatorListener(); sal != nil {
			sal.OnMaxPublishedSpatialChanged(d)
		}
	}
}

func (d *DownTrack) UpTrackMaxTemporalLayerSeenChange(maxTemporalLayerSeen int32) {
	if d.forwarder.SetMaxTemporalLayerSeen(maxTemporalLayerSeen) {
		if sal := d.getStreamAllocatorListener(); sal != nil {
			sal.OnMaxPublishedTemporalChanged(d)
		}
	}
}

func (d *DownTrack) maybeAddTransition(bitrate int64, distance float64, pauseReason VideoPauseReason) {
	if d.kind == webrtc.RTPCodecTypeAudio {
		return
	}

	if pauseReason == VideoPauseReasonBandwidth {
		d.connectionStats.UpdatePause(true)
	} else {
		d.connectionStats.UpdatePause(false)
		d.connectionStats.AddLayerTransition(distance)
		d.connectionStats.AddBitrateTransition(bitrate)
	}
}

func (d *DownTrack) UpTrackBitrateReport(availableLayers []int32, bitrates Bitrates) {
	d.maybeAddTransition(
		d.forwarder.GetOptimalBandwidthNeeded(bitrates),
		d.forwarder.DistanceToDesired(availableLayers, bitrates),
		d.forwarder.PauseReason(),
	)
}

// OnCloseHandler method to be called on remote tracked removed
func (d *DownTrack) OnCloseHandler(fn func(isExpectedToResume bool)) {
	d.cbMu.Lock()
	defer d.cbMu.Unlock()

	d.onCloseHandler = fn
}

func (d *DownTrack) getOnCloseHandler() func(isExpectedToResume bool) {
	d.cbMu.RLock()
	defer d.cbMu.RUnlock()

	return d.onCloseHandler
}

func (d *DownTrack) OnBinding(fn func(error)) {
	d.bindLock.Lock()
	defer d.bindLock.Unlock()

	d.onBinding = fn
}

func (d *DownTrack) AddReceiverReportListener(listener ReceiverReportListener) {
	d.listenerLock.Lock()
	defer d.listenerLock.Unlock()

	d.receiverReportListeners = append(d.receiverReportListeners, listener)
}

func (d *DownTrack) OnStatsUpdate(fn func(dt *DownTrack, stat *livekit.AnalyticsStat)) {
	d.cbMu.Lock()
	defer d.cbMu.Unlock()

	d.onStatsUpdate = fn
}

func (d *DownTrack) getOnStatsUpdate() func(dt *DownTrack, stat *livekit.AnalyticsStat) {
	d.cbMu.RLock()
	defer d.cbMu.RUnlock()

	return d.onStatsUpdate
}

func (d *DownTrack) OnRttUpdate(fn func(dt *DownTrack, rtt uint32)) {
	d.cbMu.Lock()
	defer d.cbMu.Unlock()

	d.onRttUpdate = fn
}

func (d *DownTrack) getOnRttUpdate() func(dt *DownTrack, rtt uint32) {
	d.cbMu.RLock()
	defer d.cbMu.RUnlock()

	return d.onRttUpdate
}

func (d *DownTrack) OnMaxLayerChanged(fn func(dt *DownTrack, layer int32)) {
	d.cbMu.Lock()
	defer d.cbMu.Unlock()

	d.onMaxSubscribedLayerChanged = fn
}

func (d *DownTrack) getOnMaxLayerChanged() func(dt *DownTrack, layer int32) {
	d.cbMu.RLock()
	defer d.cbMu.RUnlock()

	return d.onMaxSubscribedLayerChanged
}

func (d *DownTrack) IsDeficient() bool {
	return d.forwarder.IsDeficient()
}

func (d *DownTrack) BandwidthRequested() int64 {
	_, brs := d.params.Receiver.GetLayeredBitrate()
	return d.forwarder.BandwidthRequested(brs)
}

func (d *DownTrack) DistanceToDesired() float64 {
	al, brs := d.params.Receiver.GetLayeredBitrate()
	return d.forwarder.DistanceToDesired(al, brs)
}

func (d *DownTrack) AllocateOptimal(allowOvershoot bool) VideoAllocation {
	al, brs := d.params.Receiver.GetLayeredBitrate()
	allocation := d.forwarder.AllocateOptimal(al, brs, allowOvershoot)
	d.postKeyFrameRequestEvent()
	d.maybeAddTransition(allocation.BandwidthNeeded, allocation.DistanceToDesired, allocation.PauseReason)
	return allocation
}

func (d *DownTrack) ProvisionalAllocatePrepare() {
	al, brs := d.params.Receiver.GetLayeredBitrate()
	d.forwarder.ProvisionalAllocatePrepare(al, brs)
}

func (d *DownTrack) ProvisionalAllocateReset() {
	d.forwarder.ProvisionalAllocateReset()
}

func (d *DownTrack) ProvisionalAllocate(availableChannelCapacity int64, layers buffer.VideoLayer, allowPause bool, allowOvershoot bool) (bool, int64) {
	return d.forwarder.ProvisionalAllocate(availableChannelCapacity, layers, allowPause, allowOvershoot)
}

func (d *DownTrack) ProvisionalAllocateGetCooperativeTransition(allowOvershoot bool) VideoTransition {
	transition, availableLayers, brs := d.forwarder.ProvisionalAllocateGetCooperativeTransition(allowOvershoot)
	d.params.Logger.Debugw(
		"stream: cooperative transition",
		"transition", &transition,
		"availableLayers", availableLayers,
		"bitrates", brs,
	)
	return transition
}

func (d *DownTrack) ProvisionalAllocateGetBestWeightedTransition() VideoTransition {
	transition, availableLayers, brs := d.forwarder.ProvisionalAllocateGetBestWeightedTransition()
	d.params.Logger.Debugw(
		"stream: best weighted transition",
		"transition", &transition,
		"availableLayers", availableLayers,
		"bitrates", brs,
	)
	return transition
}

func (d *DownTrack) ProvisionalAllocateCommit() VideoAllocation {
	allocation := d.forwarder.ProvisionalAllocateCommit()
	d.postKeyFrameRequestEvent()
	d.maybeAddTransition(allocation.BandwidthNeeded, allocation.DistanceToDesired, allocation.PauseReason)
	return allocation
}

func (d *DownTrack) AllocateNextHigher(availableChannelCapacity int64, allowOvershoot bool) (VideoAllocation, bool) {
	al, brs := d.params.Receiver.GetLayeredBitrate()
	allocation, available := d.forwarder.AllocateNextHigher(availableChannelCapacity, al, brs, allowOvershoot)
	d.postKeyFrameRequestEvent()
	d.maybeAddTransition(allocation.BandwidthNeeded, allocation.DistanceToDesired, allocation.PauseReason)
	return allocation, available
}

func (d *DownTrack) GetNextHigherTransition(allowOvershoot bool) (VideoTransition, bool) {
	availableLayers, brs := d.params.Receiver.GetLayeredBitrate()
	transition, available := d.forwarder.GetNextHigherTransition(brs, allowOvershoot)
	d.params.Logger.Debugw(
		"stream: get next higher layer",
		"transition", transition,
		"available", available,
		"availableLayers", availableLayers,
		"bitrates", brs,
	)
	return transition, available
}

func (d *DownTrack) Pause() VideoAllocation {
	al, brs := d.params.Receiver.GetLayeredBitrate()
	allocation := d.forwarder.Pause(al, brs)
	d.maybeAddTransition(allocation.BandwidthNeeded, allocation.DistanceToDesired, allocation.PauseReason)
	return allocation
}

func (d *DownTrack) Resync() {
	d.forwarder.Resync()
}

func (d *DownTrack) CreateSourceDescriptionChunks() []rtcp.SourceDescriptionChunk {
	transceiver := d.transceiver.Load()
	if !d.bound.Load() || transceiver == nil {
		return nil
	}
	return []rtcp.SourceDescriptionChunk{
		{
			Source: d.ssrc,
			Items: []rtcp.SourceDescriptionItem{
				{
					Type: rtcp.SDESCNAME,
					Text: d.params.StreamID,
				},
				{
					Type: rtcp.SDESType(15),
					Text: transceiver.Mid(),
				},
			},
		},
	}
}

func (d *DownTrack) CreateSenderReport() *rtcp.SenderReport {
	if !d.bound.Load() {
		return nil
	}

	_, tsOffset, refSenderReport := d.forwarder.GetSenderReportParams()
	return d.rtpStats.GetRtcpSenderReport(d.ssrc, refSenderReport, tsOffset)
}

func (d *DownTrack) writeBlankFrameRTP(duration float32, generation uint32) chan struct{} {
	done := make(chan struct{})
	go func() {
		// don't send if not writable OR nothing has been sent
		if !d.writable.Load() || !d.rtpStats.IsActive() {
			close(done)
			return
		}

		var getBlankFrame func(bool) ([]byte, error)
		switch d.mime {
		case "audio/opus":
			getBlankFrame = d.getOpusBlankFrame
		case "audio/red":
			getBlankFrame = d.getOpusRedBlankFrame
		case "video/vp8":
			getBlankFrame = d.getVP8BlankFrame
		case "video/h264":
			getBlankFrame = d.getH264BlankFrame
		default:
			close(done)
			return
		}

		frameRate := uint32(30)
		if d.mime == "audio/opus" || d.mime == "audio/red" {
			frameRate = 50
		}

		// send a number of blank frames just in case there is loss.
		// Intentionally ignoring check for mute or bandwidth constrained mute
		// as this is used to clear client side buffer.
		numFrames := int(float32(frameRate) * duration)
		frameDuration := time.Duration(1000/frameRate) * time.Millisecond

		ticker := time.NewTicker(frameDuration)
		defer ticker.Stop()

		for {
			if generation != d.blankFramesGeneration.Load() || numFrames <= 0 || !d.writable.Load() || !d.rtpStats.IsActive() {
				close(done)
				return
			}

			snts, frameEndNeeded, err := d.forwarder.GetSnTsForBlankFrames(frameRate, 1)
			if err != nil {
				d.params.Logger.Warnw("could not get SN/TS for blank frame", err)
				close(done)
				return
			}

			for i := 0; i < len(snts); i++ {
				hdr := rtp.Header{
					Version:        2,
					Padding:        false,
					Marker:         true,
					PayloadType:    d.payloadType,
					SequenceNumber: uint16(snts[i].extSequenceNumber),
					Timestamp:      uint32(snts[i].extTimestamp),
					SSRC:           d.ssrc,
					CSRC:           []uint32{},
				}

				payload, err := getBlankFrame(frameEndNeeded)
				if err != nil {
					d.params.Logger.Warnw("could not get blank frame", err)
					close(done)
					return
				}

				d.sendingPacket(&hdr, len(payload), &sendPacketMetadata{
					packetTime:        time.Now(),
					extSequenceNumber: snts[i].extSequenceNumber,
					extTimestamp:      snts[i].extTimestamp,
				})
				d.pacer.Enqueue(pacer.Packet{
					Header:             &hdr,
					Payload:            payload,
					AbsSendTimeExtID:   uint8(d.absSendTimeExtID),
					TransportWideExtID: uint8(d.transportWideExtID),
					WriteStream:        d.writeStream,
				})

				// only the first frame will need frameEndNeeded to close out the
				// previous picture, rest are small key frames (for the video case)
				frameEndNeeded = false
			}

			numFrames--
			<-ticker.C
		}
	}()

	return done
}

func (d *DownTrack) maybeAddTrailer(buf []byte) int {
	if len(buf) < len(d.params.Trailer) {
		d.params.Logger.Warnw("trailer too big", nil, "bufLen", len(buf), "trailerLen", len(d.params.Trailer))
		return 0
	}

	copy(buf, d.params.Trailer)
	return len(d.params.Trailer)
}

func (d *DownTrack) getOpusBlankFrame(_frameEndNeeded bool) ([]byte, error) {
	// silence frame
	// Used shortly after muting to ensure residual noise does not keep
	// generating noise at the decoder after the stream is stopped
	// i. e. comfort noise generation actually not producing something comfortable.
	payload := make([]byte, 1000)
	copy(payload[0:], OpusSilenceFrame)
	trailerLen := d.maybeAddTrailer(payload[len(OpusSilenceFrame):])
	return payload[:len(OpusSilenceFrame)+trailerLen], nil
}

func (d *DownTrack) getOpusRedBlankFrame(_frameEndNeeded bool) ([]byte, error) {
	// primary only silence frame for opus/red, there is no need to contain redundant silent frames
	payload := make([]byte, 1000)

	// primary header
	//  0 1 2 3 4 5 6 7
	// +-+-+-+-+-+-+-+-+
	// |0|   Block PT  |
	// +-+-+-+-+-+-+-+-+
	payload[0] = opusPT
	copy(payload[1:], OpusSilenceFrame)
	trailerLen := d.maybeAddTrailer(payload[1+len(OpusSilenceFrame):])
	return payload[:1+len(OpusSilenceFrame)+trailerLen], nil
}

func (d *DownTrack) getVP8BlankFrame(frameEndNeeded bool) ([]byte, error) {
	// 8x8 key frame
	// Used even when closing out a previous frame. Looks like receivers
	// do not care about content (it will probably end up being an undecodable
	// frame, but that should be okay as there are key frames following)
	header, err := d.forwarder.GetPadding(frameEndNeeded)
	if err != nil {
		return nil, err
	}

	payload := make([]byte, 1000)
	copy(payload, header)
	copy(payload[len(header):], VP8KeyFrame8x8)
	trailerLen := d.maybeAddTrailer(payload[len(header)+len(VP8KeyFrame8x8):])
	return payload[:len(header)+len(VP8KeyFrame8x8)+trailerLen], nil
}

func (d *DownTrack) getH264BlankFrame(_frameEndNeeded bool) ([]byte, error) {
	// TODO - Jie Zeng
	// now use STAP-A to compose sps, pps, idr together, most decoder support packetization-mode 1.
	// if client only support packetization-mode 0, use single nalu unit packet
	buf := make([]byte, 1000)
	offset := 0
	buf[0] = 0x18 // STAP-A
	offset++
	for _, payload := range H264KeyFrame2x2 {
		binary.BigEndian.PutUint16(buf[offset:], uint16(len(payload)))
		offset += 2
		copy(buf[offset:offset+len(payload)], payload)
		offset += len(payload)
	}
	offset += d.maybeAddTrailer(buf[offset:])
	return buf[:offset], nil
}

func (d *DownTrack) handleRTCP(bytes []byte) {
	pkts, err := rtcp.Unmarshal(bytes)
	if err != nil {
		d.params.Logger.Errorw("could not unmarshal rtcp receiver packets", err)
		return
	}

	pliOnce := true
	sendPliOnce := func() {
		_, layer := d.forwarder.CheckSync()
		if pliOnce {
			if layer != buffer.InvalidLayerSpatial {
				d.params.Logger.Debugw("sending PLI RTCP", "layer", layer)
				d.params.Receiver.SendPLI(layer, false)
				d.isNACKThrottled.Store(true)
				d.rtpStats.UpdatePliTime()
				pliOnce = false
			}
		}
	}

	rttToReport := uint32(0)

	var numNACKs uint32
	var numPLIs uint32
	var numFIRs uint32
	for _, pkt := range pkts {
		switch p := pkt.(type) {
		case *rtcp.PictureLossIndication:
			if p.MediaSSRC == d.ssrc {
				numPLIs++
				sendPliOnce()
			}

		case *rtcp.FullIntraRequest:
			if p.MediaSSRC == d.ssrc {
				numFIRs++
				sendPliOnce()
			}

		case *rtcp.ReceiverEstimatedMaximumBitrate:
			if sal := d.getStreamAllocatorListener(); sal != nil {
				sal.OnREMB(d, p)
			}

		case *rtcp.ReceiverReport:
			// create new receiver report w/ only valid reception reports
			rr := &rtcp.ReceiverReport{
				SSRC:              p.SSRC,
				ProfileExtensions: p.ProfileExtensions,
			}
			for _, r := range p.Reports {
				if r.SSRC != d.ssrc {
					continue
				}
				rr.Reports = append(rr.Reports, r)

				rtt, isRttChanged := d.rtpStats.UpdateFromReceiverReport(r)
				if isRttChanged {
					rttToReport = rtt
				}

				/* STREAM-ALLOCATOR-DATA
				if sal := d.getStreamAllocatorListener(); sal != nil {
					sal.OnRTCPReceiverReport(d, r)
				}
				*/

				if d.playoutDelay != nil {
					d.playoutDelay.OnSeqAcked(uint16(r.LastSequenceNumber))
					// screen share track has inaccuracy jitter due to its low frame rate and bursty traffic
					if d.params.Source != livekit.TrackSource_SCREEN_SHARE {
						jitterMs := uint64(r.Jitter*1e3) / uint64(d.codec.ClockRate)
						d.playoutDelay.SetJitter(uint32(jitterMs))
					}
				}
			}
			if len(rr.Reports) > 0 {
				d.listenerLock.RLock()
				for _, l := range d.receiverReportListeners {
					l(d, rr)
				}
				d.listenerLock.RUnlock()
			}

		case *rtcp.TransportLayerNack:
			if p.MediaSSRC == d.ssrc {
				var nacks []uint16
				for _, pair := range p.Nacks {
					packetList := pair.PacketList()
					numNACKs += uint32(len(packetList))
					nacks = append(nacks, packetList...)
				}
				go d.retransmitPackets(nacks)
			}

		case *rtcp.TransportLayerCC:
			if p.MediaSSRC == d.ssrc {
				if sal := d.getStreamAllocatorListener(); sal != nil {
					sal.OnTransportCCFeedback(d, p)
				}
			}

		case *rtcp.ExtendedReport:
			// SFU only responds with the DLRRReport for the track has the sender SSRC, the behavior is different with
			// browser's implementation, which includes all sent tracks. It is ok since all the tracks
			// use the same connection, and server-sdk-go can get the rtt from the first DLRRReport
			// (libwebrtc/browsers don't send XR to calculate rtt, it only responds)
			var lastRR uint32
			for _, report := range p.Reports {
				if rr, ok := report.(*rtcp.ReceiverReferenceTimeReportBlock); ok {
					lastRR = uint32(rr.NTPTimestamp >> 16)
					break
				}
			}

			if lastRR > 0 {
				d.params.RTCPWriter([]rtcp.Packet{&rtcp.ExtendedReport{
					SenderSSRC: d.ssrc,
					Reports: []rtcp.ReportBlock{
						&rtcp.DLRRReportBlock{
							Reports: []rtcp.DLRRReport{{
								SSRC:   p.SenderSSRC,
								LastRR: lastRR,
								DLRR:   0, // no delay
							}},
						},
					},
				}})
			}
		}
	}

	d.rtpStats.UpdateNack(numNACKs)
	d.rtpStats.UpdatePli(numPLIs)
	d.rtpStats.UpdateFir(numFIRs)

	if rttToReport != 0 {
		if d.sequencer != nil {
			d.sequencer.setRTT(rttToReport)
		}

		if onRttUpdate := d.getOnRttUpdate(); onRttUpdate != nil {
			onRttUpdate(d, rttToReport)
		}
	}
}

func (d *DownTrack) SetConnected() {
	d.bindLock.Lock()
	if !d.connected.Swap(true) {
		d.onBindAndConnectedChange()
	}
	d.params.Logger.Debugw("downtrack connected")
	d.bindLock.Unlock()
}

// SetActivePaddingOnMuteUpTrack will enable padding on the track when its uptrack is muted.
// Pion will not fire OnTrack event until it receives packet for the track,
// so we send padding packets to help pion client (go-sdk) to fire the event.
func (d *DownTrack) SetActivePaddingOnMuteUpTrack() {
	d.activePaddingOnMuteUpTrack.Store(true)
}

func (d *DownTrack) retransmitPackets(nacks []uint16) {
	if d.sequencer == nil {
		return
	}

	if FlagStopRTXOnPLI && d.isNACKThrottled.Load() {
		return
	}

	filtered, disallowedLayers := d.forwarder.FilterRTX(nacks)
	if len(filtered) == 0 {
		return
	}

	src := PacketFactory.Get().(*[]byte)
	defer PacketFactory.Put(src)

	nackAcks := uint32(0)
	nackMisses := uint32(0)
	numRepeatedNACKs := uint32(0)
	// STREAM-ALLOCATOR-DATA nackInfos := make([]NackInfo, 0, len(filtered))
	for _, epm := range d.sequencer.getExtPacketMetas(filtered) {
		if disallowedLayers[epm.layer] {
			continue
		}

		nackAcks++
		/* STREAM-ALLOCATOR-DATA
		nackInfos = append(nackInfos, NackInfo{
			SequenceNumber: epm.targetSeqNo,
			Timestamp:      epm.timestamp,
			Attempts:       epm.nacked,
		})
		*/

		pktBuff := *src
		n, err := d.params.Receiver.ReadRTP(pktBuff, uint8(epm.layer), epm.sourceSeqNo)
		if err != nil {
			if err == io.EOF {
				break
			}
			nackMisses++
			continue
		}

		if epm.nacked > 1 {
			numRepeatedNACKs++
		}

		var pkt rtp.Packet
		if err = pkt.Unmarshal(pktBuff[:n]); err != nil {
			d.params.Logger.Errorw("could not unmarshal rtp packet in retransmit", err)
			continue
		}
		pkt.Header.Marker = epm.marker
		pkt.Header.SequenceNumber = epm.targetSeqNo
		pkt.Header.Timestamp = epm.timestamp
		pkt.Header.SSRC = d.ssrc
		pkt.Header.PayloadType = d.payloadType

		poolEntity := PacketFactory.Get().(*[]byte)
		payload := *poolEntity
		if len(epm.codecBytesSlice) != 0 {
			n := copy(payload, epm.codecBytesSlice)
			m := copy(payload[n:], pkt.Payload[epm.numCodecBytesIn:])
			payload = payload[:n+m]
		} else {
			copy(payload, epm.codecBytes[:epm.numCodecBytesOut])
			copy(payload[epm.numCodecBytesOut:], pkt.Payload[epm.numCodecBytesIn:])
			payload = payload[:int(epm.numCodecBytesOut)+len(pkt.Payload)-int(epm.numCodecBytesIn)]
		}

		var extensions []pacer.ExtensionData
		if d.dependencyDescriptorExtID != 0 {
			var ddBytes []byte
			if len(epm.ddBytesSlice) != 0 {
				ddBytes = epm.ddBytesSlice
			} else {
				ddBytes = epm.ddBytes[:epm.ddBytesSize]
			}
			extensions = append(
				extensions,
				pacer.ExtensionData{
					ID:      uint8(d.dependencyDescriptorExtID),
					Payload: ddBytes,
				},
			)
		}
		if d.absCaptureTimeExtID != 0 && len(epm.actBytes) != 0 {
			extensions = append(
				extensions,
				pacer.ExtensionData{
					ID:      uint8(d.absCaptureTimeExtID),
					Payload: epm.actBytes,
				},
			)
		}

		d.sendingPacket(
			&pkt.Header,
			len(payload),
			&sendPacketMetadata{
				layer:             int32(epm.layer),
				packetTime:        time.Now(),
				extSequenceNumber: epm.extSequenceNumber,
				extTimestamp:      epm.extTimestamp,
				isRTX:             true,
			},
		)
		d.pacer.Enqueue(pacer.Packet{
			Header:             &pkt.Header,
			Extensions:         extensions,
			Payload:            payload,
			AbsSendTimeExtID:   uint8(d.absSendTimeExtID),
			TransportWideExtID: uint8(d.transportWideExtID),
			WriteStream:        d.writeStream,
			Pool:               PacketFactory,
			PoolEntity:         poolEntity,
		})
	}

	d.totalRepeatedNACKs.Add(numRepeatedNACKs)

	d.rtpStats.UpdateNackProcessed(nackAcks, nackMisses, numRepeatedNACKs)
	/* STREAM-ALLOCATOR-DATA
	// STREAM-ALLOCATOR-EXPERIMENTAL-TODO-START
	// Need to check on the following
	//   - get all NACKs from sequencer even if SFU is not acknowledging,
	//     i. e. SFU does not acknowledge even same sequence number is NACKed too closely,
	//     but if sequencer return those also (even if not actually retransmitting),
	//     will that provide a signal?
	//   - get padding NACKs also? Maybe only look at them when their NACK count is 2?
	//     because padding runs in a separate path, it could get out of order with
	//     primary packets. So, it could be NACKed once. But, a repeat NACK means they
	//     were probably lost. But, as we do not retransmit padding packets, more than
	//     the second try does not provide any useful signal.
	// STREAM-ALLOCATOR-EXPERIMENTAL-TODO-END
	if sal := d.getStreamAllocatorListener(); sal != nil && len(nackInfos) != 0 {
		sal.OnNACK(d, nackInfos)
	}
	*/
}

func (d *DownTrack) getTranslatedRTPHeader(extPkt *buffer.ExtPacket, tp *TranslationParams) (*rtp.Header, error) {
	hdr := extPkt.Packet.Header
	hdr.PayloadType = d.payloadType
	hdr.Timestamp = uint32(tp.rtp.extTimestamp)
	hdr.SequenceNumber = uint16(tp.rtp.extSequenceNumber)
	hdr.SSRC = d.ssrc
	if tp.marker {
		hdr.Marker = tp.marker
	}

	return &hdr, nil
}

func (d *DownTrack) DebugInfo() map[string]interface{} {
	stats := map[string]interface{}{
		"LastPli": d.rtpStats.LastPli(),
	}
	stats["RTPMunger"] = d.forwarder.RTPMungerDebugInfo()

	senderReport := d.CreateSenderReport()
	if senderReport != nil {
		stats["NTPTime"] = senderReport.NTPTime
		stats["RTPTime"] = senderReport.RTPTime
		stats["PacketCount"] = senderReport.PacketCount
	}

	return map[string]interface{}{
		"SubscriberID":        d.params.SubID,
		"TrackID":             d.id,
		"StreamID":            d.params.StreamID,
		"SSRC":                d.ssrc,
		"MimeType":            d.codec.MimeType,
		"Bound":               d.bound.Load(),
		"Muted":               d.forwarder.IsMuted(),
		"PubMuted":            d.forwarder.IsPubMuted(),
		"CurrentSpatialLayer": d.forwarder.CurrentLayer().Spatial,
		"Stats":               stats,
	}
}

func (d *DownTrack) getExpectedRTPTimestamp(at time.Time) (uint64, error) {
	return d.rtpStats.GetExpectedRTPTimestamp(at)
}

func (d *DownTrack) GetConnectionScoreAndQuality() (float32, livekit.ConnectionQuality) {
	return d.connectionStats.GetScoreAndQuality()
}

func (d *DownTrack) GetTrackStats() *livekit.RTPStats {
	return d.rtpStats.ToProto()
}

func (d *DownTrack) deltaStats(ds *buffer.RTPDeltaInfo) map[uint32]*buffer.StreamStatsWithLayers {
	if ds == nil {
		return nil
	}

	streamStats := make(map[uint32]*buffer.StreamStatsWithLayers, 1)
	streamStats[d.ssrc] = &buffer.StreamStatsWithLayers{
		RTPStats: ds,
		Layers: map[int32]*buffer.RTPDeltaInfo{
			0: ds,
		},
	}

	return streamStats
}

func (d *DownTrack) GetDeltaStatsSender() map[uint32]*buffer.StreamStatsWithLayers {
	return d.deltaStats(d.rtpStats.DeltaInfoSender(d.deltaStatsSenderSnapshotId))
}

func (d *DownTrack) GetLastReceiverReportTime() time.Time {
	return d.rtpStats.LastReceiverReportTime()
}

func (d *DownTrack) GetTotalPacketsSent() uint64 {
	return d.rtpStats.GetTotalPacketsPrimary()
}

func (d *DownTrack) GetNackStats() (totalPackets uint32, totalRepeatedNACKs uint32) {
	totalPackets = uint32(d.rtpStats.GetTotalPacketsPrimary())
	totalRepeatedNACKs = d.totalRepeatedNACKs.Load()
	return
}

/* STREAM-ALLOCATOR-DATA
func (d *DownTrack) GetAndResetBytesSent() (uint32, uint32) {
	return d.bytesSent.Swap(0), d.bytesRetransmitted.Swap(0)
}
*/

func (d *DownTrack) onBindAndConnectedChange() {
	d.writable.Store(d.connected.Load() && d.bound.Load())
	if d.connected.Load() && d.bound.Load() && !d.bindAndConnectedOnce.Swap(true) {
		if d.activePaddingOnMuteUpTrack.Load() {
			go d.sendPaddingOnMute()
		}

		// kick off PLI request if allocation is pending
		d.postKeyFrameRequestEvent()
	}
}

func (d *DownTrack) sendPaddingOnMute() {
	// let uptrack have chance to send packet before we send padding
	time.Sleep(waitBeforeSendPaddingOnMute)

	if d.kind == webrtc.RTPCodecTypeVideo {
		d.sendPaddingOnMuteForVideo()
	} else if d.mime == "audio/opus" {
		d.sendSilentFrameOnMuteForOpus()
	}
}

func (d *DownTrack) sendPaddingOnMuteForVideo() {
	paddingOnMuteInterval := 100 * time.Millisecond
	numPackets := maxPaddingOnMuteDuration / paddingOnMuteInterval
	for i := 0; i < int(numPackets); i++ {
		if d.rtpStats.IsActive() || d.IsClosed() {
			return
		}
		if i == 0 {
			d.params.Logger.Debugw("sending padding on mute")
		}
		d.WritePaddingRTP(20, true, true)
		time.Sleep(paddingOnMuteInterval)
	}
}

func (d *DownTrack) sendSilentFrameOnMuteForOpus() {
	frameRate := uint32(50)
	frameDuration := time.Duration(1000/frameRate) * time.Millisecond
	numFrames := frameRate * uint32(maxPaddingOnMuteDuration/time.Second)
	first := true
	for {
		if d.rtpStats.IsActive() || d.IsClosed() || numFrames <= 0 {
			return
		}
		if first {
			first = false
			d.params.Logger.Debugw("sending padding on mute")
		}
		snts, _, err := d.forwarder.GetSnTsForBlankFrames(frameRate, 1)
		if err != nil {
			d.params.Logger.Warnw("could not get SN/TS for blank frame", err)
			return
		}
		for i := 0; i < len(snts); i++ {
			hdr := rtp.Header{
				Version:        2,
				Padding:        false,
				Marker:         true,
				PayloadType:    d.payloadType,
				SequenceNumber: uint16(snts[i].extSequenceNumber),
				Timestamp:      uint32(snts[i].extTimestamp),
				SSRC:           d.ssrc,
				CSRC:           []uint32{},
			}

			payload, err := d.getOpusBlankFrame(false)
			if err != nil {
				d.params.Logger.Warnw("could not get blank frame", err)
				return
			}

			d.sendingPacket(
				&hdr,
				len(payload),
				&sendPacketMetadata{
					packetTime:        time.Now(),
					extSequenceNumber: snts[i].extSequenceNumber,
					extTimestamp:      snts[i].extTimestamp,
					// although this is using empty frames, mark as padding as these are used to trigger Pion OnTrack only
					isPadding: true,
				},
			)
			d.pacer.Enqueue(pacer.Packet{
				Header:             &hdr,
				Payload:            payload,
				AbsSendTimeExtID:   uint8(d.absSendTimeExtID),
				TransportWideExtID: uint8(d.transportWideExtID),
				WriteStream:        d.writeStream,
			})
		}

		numFrames--
		time.Sleep(frameDuration)
	}
}

func (d *DownTrack) HandleRTCPSenderReportData(
	_payloadType webrtc.PayloadType,
	isSVC bool,
	layer int32,
	publisherSRData *buffer.RTCPSenderReportData,
) error {
	d.forwarder.SetRefSenderReport(isSVC, layer, publisherSRData)

	currentLayer, tsOffset, refSenderReport := d.forwarder.GetSenderReportParams()
	if layer == currentLayer || (layer == 0 && isSVC) {
		d.handleRTCPSenderReportData(refSenderReport, tsOffset)
	}
	return nil
}

func (d *DownTrack) handleRTCPSenderReportData(publisherSRData *buffer.RTCPSenderReportData, tsOffset uint64) {
	d.rtpStats.MaybeAdjustFirstPacketTime(publisherSRData, tsOffset)
}

type sendPacketMetadata struct {
	layer                int32
	packetTime           time.Time
	extSequenceNumber    uint64
	extTimestamp         uint64
	isKeyFrame           bool
	isRTX                bool
	isPadding            bool
	shouldDisableCounter bool
	tp                   *TranslationParams
}

func (d *DownTrack) sendingPacket(hdr *rtp.Header, payloadSize int, spmd *sendPacketMetadata) {
	hdrSize := hdr.MarshalSize()
	if !spmd.shouldDisableCounter {
		// STREAM-ALLOCATOR-TODO: remove this stream allocator bytes counter once stream allocator changes fully to pull bytes counter
		size := uint32(hdrSize + payloadSize)
		d.streamAllocatorBytesCounter.Add(size)
		/* STREAM-ALLOCATOR-DATA
		if spmd.isRTX {
			d.bytesRetransmitted.Add(size)
		} else {
			d.bytesSent.Add(size)
		}
		*/
	}

	// update RTPStats
	if spmd.isPadding {
		d.rtpStats.Update(spmd.packetTime, spmd.extSequenceNumber, spmd.extTimestamp, hdr.Marker, hdrSize, 0, payloadSize)
	} else {
		d.rtpStats.Update(spmd.packetTime, spmd.extSequenceNumber, spmd.extTimestamp, hdr.Marker, hdrSize, payloadSize, 0)
	}

	if spmd.isKeyFrame {
		d.isNACKThrottled.Store(false)
		d.rtpStats.UpdateKeyFrame(1)
		d.params.Logger.Debugw(
			"forwarded key frame",
			"layer", spmd.layer,
			"rtpsn", spmd.extSequenceNumber,
			"rtpts", spmd.extTimestamp,
		)
	}

	if spmd.tp != nil {
		if spmd.tp.isSwitching {
			d.postMaxLayerNotifierEvent("switching")
		}

		if spmd.tp.isResuming {
			if sal := d.getStreamAllocatorListener(); sal != nil {
				sal.OnResume(d)
			}
		}
	}
}

// -------------------------------------------------------------------------------
