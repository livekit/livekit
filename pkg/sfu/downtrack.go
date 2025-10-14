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
	"math/rand"
	"strings"
	"sync"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/sdp/v3"
	"github.com/pion/transport/v3/packetio"
	"github.com/pion/webrtc/v4"
	"go.uber.org/atomic"
	"go.uber.org/zap/zapcore"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils/mono"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/sfu/bwe"
	"github.com/livekit/livekit-server/pkg/sfu/ccutils"
	"github.com/livekit/livekit-server/pkg/sfu/connectionquality"
	"github.com/livekit/livekit-server/pkg/sfu/mime"
	"github.com/livekit/livekit-server/pkg/sfu/pacer"
	act "github.com/livekit/livekit-server/pkg/sfu/rtpextension/abscapturetime"
	dd "github.com/livekit/livekit-server/pkg/sfu/rtpextension/dependencydescriptor"
	pd "github.com/livekit/livekit-server/pkg/sfu/rtpextension/playoutdelay"
	"github.com/livekit/livekit-server/pkg/sfu/rtpstats"
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
	HandleRTCPSenderReportData(
		payloadType webrtc.PayloadType,
		layer int32,
		publisherSRData *livekit.RTCPSenderReportState,
	) error
	Resync()
	SetReceiver(TrackReceiver)
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
	paddingOnMuteInterval       = 100 * time.Millisecond
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

	dummyAbsSendTimeExt, _ = rtp.NewAbsSendTimeExtension(mono.Now()).Marshal()
	dummyTransportCCExt, _ = rtp.TransportCCExtension{TransportSequence: 12345}.Marshal()
)

// -------------------------------------------------------------------

type DownTrackState struct {
	RTPStats                      *rtpstats.RTPStatsSender
	DeltaStatsSenderSnapshotId    uint32
	RTPStatsRTX                   *rtpstats.RTPStatsSender
	DeltaStatsRTXSenderSnapshotId uint32
	ForwarderState                *livekit.RTPForwarderState
	PlayoutDelayControllerState   PlayoutDelayControllerState
}

func (d DownTrackState) MarshalLogObject(e zapcore.ObjectEncoder) error {
	e.AddObject("RTPStats", d.RTPStats)
	e.AddUint32("DeltaStatsSenderSnapshotId", d.DeltaStatsSenderSnapshotId)
	e.AddObject("RTPStatsRTX", d.RTPStatsRTX)
	e.AddUint32("DeltaStatsRTXSenderSnapshotId", d.DeltaStatsRTXSenderSnapshotId)
	e.AddObject("ForwarderState", logger.Proto(d.ForwarderState))
	e.AddObject("PlayoutDelayControllerState", d.PlayoutDelayControllerState)
	return nil
}

// -------------------------------------------------------------------

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

type DownTrackListener interface {
	OnBindAndConnected()
	OnStatsUpdate(stat *livekit.AnalyticsStat)
	OnMaxSubscribedLayerChanged(layer int32)
	OnRttUpdate(rtt uint32)
	OnCodecNegotiated(webrtc.RTPCodecCapability)
	OnDownTrackClose(isExpectedToResume bool)
}

// -------------------------------------------------------------------

type bindState int

const (
	bindStateUnbound bindState = iota
	// downtrack negotiated, but waiting for receiver to be ready to start forwarding
	bindStateWaitForReceiverReady
	// downtrack is bound and ready to forward
	bindStateBound
)

func (bs bindState) String() string {
	switch bs {
	case bindStateUnbound:
		return "unbound"
	case bindStateWaitForReceiverReady:
		return "waitForReceiverReady"
	case bindStateBound:
		return "bound"
	}
	return "unknown"
}

// -------------------------------------------------------------------

var _ TrackSender = (*DownTrack)(nil)

type ReceiverReportListener func(dt *DownTrack, report *rtcp.ReceiverReport)

type DownTrackParams struct {
	Codecs                         []webrtc.RTPCodecParameters
	IsEncrypted                    bool
	Source                         livekit.TrackSource
	Receiver                       TrackReceiver
	BufferFactory                  *buffer.Factory
	SubID                          livekit.ParticipantID
	StreamID                       string
	MaxTrack                       int
	PlayoutDelayLimit              *livekit.PlayoutDelay
	Pacer                          pacer.Pacer
	Logger                         logger.Logger
	Trailer                        []byte
	RTCPWriter                     func([]rtcp.Packet) error
	DisableSenderReportPassThrough bool
	SupportsCodecChange            bool
	Listener                       DownTrackListener
}

// DownTrack implements webrtc.TrackLocal, is the track used to write packets
// to SFU Subscriber, the track handle the packets for simple, simulcast
// and SVC Publisher.
// A DownTrack has the following lifecycle
// - new
// - bound / unbound
// - closed
// once closed, a DownTrack cannot be re-used.
type DownTrack struct {
	params            DownTrackParams
	id                livekit.TrackID
	kind              webrtc.RTPCodecType
	ssrc              uint32
	ssrcRTX           uint32
	payloadType       atomic.Uint32
	payloadTypeRTX    atomic.Uint32
	sequencer         *sequencer
	rtxSequenceNumber atomic.Uint64

	receiverLock sync.RWMutex
	receiver     TrackReceiver

	forwarder *Forwarder

	upstreamCodecs            []webrtc.RTPCodecParameters
	codec                     atomic.Value // webrtc.RTPCodecCapability
	clockRate                 uint32
	negotiatedCodecParameters []webrtc.RTPCodecParameters

	// payload types for red codec only
	isRED             bool
	upstreamPrimaryPT uint8
	primaryPT         uint8

	absSendTimeExtID          int
	transportWideExtID        int
	dependencyDescriptorExtID int
	playoutDelayExtID         int
	absCaptureTimeExtID       int
	transceiver               atomic.Pointer[webrtc.RTPTransceiver]
	writeStream               webrtc.TrackLocalWriter
	rtcpReader                *buffer.RTCPReader
	rtcpReaderRTX             *buffer.RTCPReader

	listenerLock            sync.RWMutex
	receiverReportListeners []ReceiverReportListener

	bindLock            sync.Mutex
	bindState           atomic.Value
	onBinding           func(error)
	bindOnReceiverReady func()

	isClosed             atomic.Bool
	connected            atomic.Bool
	bindAndConnectedOnce atomic.Bool
	writable             atomic.Bool
	writeStopped         atomic.Bool
	isReceiverReady      bool

	rtpStats                   *rtpstats.RTPStatsSender
	deltaStatsSenderSnapshotId uint32

	rtpStatsRTX                   *rtpstats.RTPStatsSender
	deltaStatsRTXSenderSnapshotId uint32

	totalRepeatedNACKs atomic.Uint32

	blankFramesGeneration atomic.Uint32

	connectionStats *connectionquality.ConnectionStats

	isNACKThrottled atomic.Bool

	activePaddingOnMuteUpTrack atomic.Bool

	streamAllocatorLock     sync.RWMutex
	streamAllocatorListener DownTrackStreamAllocatorListener
	probeClusterId          atomic.Uint32

	playoutDelay *PlayoutDelayController

	pacer pacer.Pacer

	maxLayerNotifierChMu     sync.RWMutex
	maxLayerNotifierCh       chan string
	maxLayerNotifierChClosed bool

	keyFrameRequesterChMu     sync.RWMutex
	keyFrameRequesterCh       chan struct{}
	keyFrameRequesterChClosed bool

	createdAt int64
}

// NewDownTrack returns a DownTrack.
func NewDownTrack(params DownTrackParams) (*DownTrack, error) {
	mimeType := mime.NormalizeMimeType(params.Codecs[0].MimeType)
	var kind webrtc.RTPCodecType
	switch {
	case mime.IsMimeTypeAudio(mimeType):
		kind = webrtc.RTPCodecTypeAudio
	case mime.IsMimeTypeVideo(mimeType):
		kind = webrtc.RTPCodecTypeVideo
	default:
		kind = webrtc.RTPCodecType(0)
	}

	codec := params.Codecs[0].RTPCodecCapability
	d := &DownTrack{
		params:              params,
		id:                  params.Receiver.TrackID(),
		upstreamCodecs:      params.Codecs,
		kind:                kind,
		clockRate:           codec.ClockRate,
		pacer:               params.Pacer,
		maxLayerNotifierCh:  make(chan string, 1),
		keyFrameRequesterCh: make(chan struct{}, 1),
		createdAt:           time.Now().UnixNano(),
		receiver:            params.Receiver,
	}
	d.codec.Store(codec)
	d.bindState.Store(bindStateUnbound)
	d.params.Logger = params.Logger.WithValues(
		"subscriberID", d.SubscriberID(),
	)

	var mdCacheSize, mdCacheSizeRTX int
	if d.kind == webrtc.RTPCodecTypeVideo {
		mdCacheSize, mdCacheSizeRTX = 8192, 8192
	} else {
		mdCacheSize, mdCacheSizeRTX = 8192, 1024
	}
	d.rtpStats = rtpstats.NewRTPStatsSender(rtpstats.RTPStatsParams{
		ClockRate: codec.ClockRate,
		Logger: d.params.Logger.WithValues(
			"stream", "primary",
		),
	}, mdCacheSize)
	d.deltaStatsSenderSnapshotId = d.rtpStats.NewSenderSnapshotId()

	d.rtpStatsRTX = rtpstats.NewRTPStatsSender(rtpstats.RTPStatsParams{
		ClockRate: codec.ClockRate,
		IsRTX:     true,
		Logger: d.params.Logger.WithValues(
			"stream", "rtx",
		),
	}, mdCacheSizeRTX)
	d.deltaStatsRTXSenderSnapshotId = d.rtpStatsRTX.NewSenderSnapshotId()

	d.forwarder = NewForwarder(
		d.kind,
		d.params.Logger,
		false,
		d.rtpStats,
	)

	d.connectionStats = connectionquality.NewConnectionStats(connectionquality.ConnectionStatsParams{
		SenderProvider: d,
		Logger:         d.params.Logger.WithValues("direction", "down"),
	})
	d.connectionStats.OnStatsUpdate(func(_cs *connectionquality.ConnectionStats, stat *livekit.AnalyticsStat) {
		d.params.Listener.OnStatsUpdate(stat)
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

	d.params.Receiver.AddOnReady(d.handleReceiverReady)
	d.rtxSequenceNumber.Store(uint64(rand.Intn(1<<14)) + uint64(1<<15)) // a random number in third quartile of sequence number space
	d.params.Logger.Debugw("downtrack created", "upstreamCodecs", d.upstreamCodecs)

	return d, nil
}

// Bind is called by the PeerConnection after negotiation is complete
// This asserts that the code requested is supported by the remote peer.
// If so it sets up all the state (SSRC and PayloadType) to have a call
func (d *DownTrack) Bind(t webrtc.TrackLocalContext) (webrtc.RTPCodecParameters, error) {
	d.bindLock.Lock()
	if d.bindState.Load() != bindStateUnbound {
		d.bindLock.Unlock()
		return webrtc.RTPCodecParameters{}, ErrDownTrackAlreadyBound
	}

	// the TrackLocalContext's codec parameters will be set to the bound codec after Bind returns,
	// so keep a copy of the codec parameters here to use it later
	d.negotiatedCodecParameters = append([]webrtc.RTPCodecParameters{}, t.CodecParameters()...)
	var codec, matchedUpstreamCodec webrtc.RTPCodecParameters
	for _, c := range d.upstreamCodecs {
		matchCodec, err := utils.CodecParametersFuzzySearch(c, d.negotiatedCodecParameters)
		if err == nil {
			codec = matchCodec
			matchedUpstreamCodec = c
			break
		} else {
			// for encrypyted tracks, should match on primary codec,
			// i. e. codec at index 0 if the combination of upstream codecs is opus and RED
			if d.params.IsEncrypted {
				isRedAndOpus := true
				for _, u := range d.upstreamCodecs {
					if !mime.IsMimeTypeStringOpus(u.MimeType) || !mime.IsMimeTypeStringRED(u.MimeType) {
						isRedAndOpus = false
						break
					}
				}
				if isRedAndOpus {
					break
				}
			}
		}
	}

	if codec.MimeType == "" {
		err := webrtc.ErrUnsupportedCodec
		onBinding := d.onBinding
		d.bindLock.Unlock()
		d.params.Logger.Infow(
			"bind error for unsupported codec",
			"codecs", d.upstreamCodecs,
			"remoteParameters", d.negotiatedCodecParameters,
		)
		if onBinding != nil {
			onBinding(err)
		}
		// don't return error here, as pion will not start transports if Bind fails at first answer
		return webrtc.RTPCodecParameters{}, nil
	}

	// if a downtrack is closed before bind, it already unsubscribed from client, don't do subsequent operation and return here.
	if d.IsClosed() {
		d.params.Logger.Debugw("DownTrack closed before bind")
		d.bindLock.Unlock()
		return codec, nil
	}

	// Bind is called under RTPSender.mu lock,
	// call the RTPSender.GetParameters (which setRTPHeaderExtensions invokes)
	// in goroutine to avoid deadlock
	go d.setRTPHeaderExtensions()

	doBind := func() {
		d.bindLock.Lock()
		if d.IsClosed() {
			d.bindLock.Unlock()
			d.params.Logger.Debugw("DownTrack closed before bind")
			return
		}

		isFECEnabled := false
		if mime.IsMimeTypeStringRED(matchedUpstreamCodec.MimeType) {
			d.isRED = true
			for _, c := range d.upstreamCodecs {
				isFECEnabled = strings.Contains(strings.ToLower(c.SDPFmtpLine), "useinbandfec=1")

				// assume upstream primary codec is opus since we only support it for audio now
				if mime.IsMimeTypeStringOpus(c.MimeType) {
					d.upstreamPrimaryPT = uint8(c.PayloadType)
					break
				}
			}
			if d.upstreamPrimaryPT == 0 {
				d.params.Logger.Errorw(
					"failed to find upstream primary opus payload type for RED", nil,
					"matchedCodec", codec,
					"upstreamCodec", d.upstreamCodecs,
				)
			}

			var primaryPT, secondaryPT int
			if n, err := fmt.Sscanf(codec.SDPFmtpLine, "%d/%d", &primaryPT, &secondaryPT); err != nil || n != 2 {
				d.params.Logger.Errorw(
					"failed to parse primary and secondary payload type for RED", err,
					"matchedCodec", codec,
				)
			}
			d.primaryPT = uint8(primaryPT)
		} else if mime.IsMimeTypeStringAudio(matchedUpstreamCodec.MimeType) {
			isFECEnabled = strings.Contains(strings.ToLower(matchedUpstreamCodec.SDPFmtpLine), "fec")
		}

		logFields := []interface{}{
			"codecs", d.upstreamCodecs,
			"matchCodec", codec,
			"ssrc", t.SSRC(),
			"ssrcRTX", t.SSRCRetransmission(),
			"isFECEnabled", isFECEnabled,
		}
		if d.isRED {
			logFields = append(
				logFields,
				"isRED", d.isRED,
				"upstreamPrimaryPT", d.upstreamPrimaryPT,
				"primaryPT", d.primaryPT,
			)
		}

		d.ssrc = uint32(t.SSRC())
		d.ssrcRTX = uint32(t.SSRCRetransmission())
		d.payloadType.Store(uint32(codec.PayloadType))
		d.payloadTypeRTX.Store(uint32(utils.FindRTXPayloadType(codec.PayloadType, d.negotiatedCodecParameters)))
		logFields = append(
			logFields,
			"payloadType", d.payloadType.Load(),
			"payloadTypeRTX", d.payloadTypeRTX.Load(),
			"codecParameters", d.negotiatedCodecParameters,
		)
		d.params.Logger.Debugw("DownTrack.Bind", logFields...)

		d.writeStream = t.WriteStream()
		if rr := d.params.BufferFactory.GetOrNew(packetio.RTCPBufferPacket, d.ssrc).(*buffer.RTCPReader); rr != nil {
			rr.OnPacket(func(pkt []byte) {
				d.handleRTCP(pkt)
			})
			d.rtcpReader = rr
		}
		if d.ssrcRTX != 0 {
			if rr := d.params.BufferFactory.GetOrNew(packetio.RTCPBufferPacket, d.ssrcRTX).(*buffer.RTCPReader); rr != nil {
				rr.OnPacket(func(pkt []byte) {
					d.handleRTCPRTX(pkt)
				})
				d.rtcpReaderRTX = rr
			}
		}

		d.sequencer = newSequencer(d.params.MaxTrack, d.kind == webrtc.RTPCodecTypeVideo, d.params.Logger)

		d.codec.Store(codec.RTPCodecCapability)
		if d.onBinding != nil {
			d.onBinding(nil)
		}
		d.setBindStateLocked(bindStateBound)
		d.bindLock.Unlock()

		receiver := d.Receiver()
		d.forwarder.DetermineCodec(codec.RTPCodecCapability, receiver.HeaderExtensions(), receiver.VideoLayerMode())
		d.connectionStats.Start(d.Mime(), isFECEnabled)
		d.params.Logger.Debugw("downtrack bound")
	}

	isReceiverReady := d.isReceiverReady
	if !isReceiverReady {
		d.params.Logger.Debugw("downtrack bound: receiver not ready", "codec", codec)
		d.bindOnReceiverReady = doBind
		d.setBindStateLocked(bindStateWaitForReceiverReady)
	}
	d.bindLock.Unlock()

	d.params.Listener.OnCodecNegotiated(codec.RTPCodecCapability)

	if isReceiverReady {
		doBind()
	}
	return codec, nil
}

func (d *DownTrack) setBindStateLocked(state bindState) {
	if d.bindState.Swap(state) == state {
		return
	}

	if state == bindStateBound || state == bindStateUnbound {
		d.bindOnReceiverReady = nil
		d.onBindAndConnectedChange()
	}
}

func (d *DownTrack) handleReceiverReady() {
	d.bindLock.Lock()
	if d.isReceiverReady {
		d.bindLock.Unlock()
		return
	}
	d.params.Logger.Debugw("downtrack receiver ready")
	d.isReceiverReady = true
	doBind := d.bindOnReceiverReady
	d.bindOnReceiverReady = nil
	d.bindLock.Unlock()

	if doBind != nil {
		doBind()
	}
}

func (d *DownTrack) handleUpstreamCodecChange(mimeType string) {
	d.bindLock.Lock()
	existingMimeType := d.codec.Load().(webrtc.RTPCodecCapability).MimeType
	if mime.IsMimeTypeStringEqual(existingMimeType, mimeType) {
		d.bindLock.Unlock()
		return
	}

	if !d.params.SupportsCodecChange {
		d.bindLock.Unlock()
		d.params.Logger.Infow("client doesn't support codec change, renegotiate new codec")
		go d.Close()
		return
	}

	oldPT, oldRtxPT, oldCodec := d.payloadType.Load(), d.payloadTypeRTX.Load(), d.codec

	var codec webrtc.RTPCodecParameters
	for _, c := range d.upstreamCodecs {
		if !mime.IsMimeTypeStringEqual(c.MimeType, mimeType) {
			continue
		}

		matchCodec, err := utils.CodecParametersFuzzySearch(c, d.negotiatedCodecParameters)
		if err == nil {
			codec = matchCodec
			break
		}
	}

	if codec.MimeType == "" {
		// codec not found, should not happen since the upstream codec should only fall back to higher compatibility (vp8)
		d.params.Logger.Errorw(
			"can't find matched codec for new upstream payload type", nil,
			"upstreamCodecs", d.upstreamCodecs,
			"remoteParameters", d.negotiatedCodecParameters,
			"mime", mimeType,
		)
		d.bindLock.Unlock()
		return
	}

	d.payloadType.Store(uint32(codec.PayloadType))
	d.payloadTypeRTX.Store(uint32(utils.FindRTXPayloadType(codec.PayloadType, d.negotiatedCodecParameters)))
	d.codec.Store(codec.RTPCodecCapability)
	isFECEnabled := strings.Contains(strings.ToLower(codec.SDPFmtpLine), "fec")
	d.bindLock.Unlock()

	d.params.Logger.Infow(
		"upstream codec changed",
		"oldPT", oldPT, "newPT", d.payloadType.Load(),
		"oldRTXPT", oldRtxPT, "newRTXPT", d.payloadTypeRTX.Load(),
		"oldCodec", oldCodec, "newCodec", codec.RTPCodecCapability,
	)

	d.forwarder.Restart()
	receiver := d.Receiver()
	d.forwarder.DetermineCodec(codec.RTPCodecCapability, receiver.HeaderExtensions(), receiver.VideoLayerMode())
	d.connectionStats.UpdateCodec(d.Mime(), isFECEnabled)
}

// Unbind implements the teardown logic when the track is no longer needed. This happens
// because a track has been stopped.
func (d *DownTrack) Unbind(_ webrtc.TrackLocalContext) error {
	d.bindLock.Lock()
	d.setBindStateLocked(bindStateUnbound)
	d.bindLock.Unlock()
	return nil
}

func (d *DownTrack) SetStreamAllocatorListener(listener DownTrackStreamAllocatorListener) {
	d.streamAllocatorLock.Lock()
	d.streamAllocatorListener = listener
	d.streamAllocatorLock.Unlock()

	d.setRTPHeaderExtensions()

	if listener != nil {
		// kick off a gratuitous allocation
		listener.OnSubscriptionChanged(d)
	}
}

func (d *DownTrack) getStreamAllocatorListener() DownTrackStreamAllocatorListener {
	d.streamAllocatorLock.RLock()
	defer d.streamAllocatorLock.RUnlock()

	return d.streamAllocatorListener
}

func (d *DownTrack) SetProbeClusterId(probeClusterId ccutils.ProbeClusterId) {
	d.probeClusterId.Store(uint32(probeClusterId))
}

func (d *DownTrack) SwapProbeClusterId(match ccutils.ProbeClusterId, swap ccutils.ProbeClusterId) {
	d.probeClusterId.CompareAndSwap(uint32(match), uint32(swap))
}

// ID is the unique identifier for this Track. This should be unique for the
// stream, but doesn't have to globally unique. A common example would be 'audio' or 'video'
// and StreamID would be 'desktop' or 'webcam'
func (d *DownTrack) ID() string { return string(d.id) }

// Codec returns current track codec capability
func (d *DownTrack) Codec() webrtc.RTPCodecCapability {
	return d.codec.Load().(webrtc.RTPCodecCapability)
}

func (d *DownTrack) Mime() mime.MimeType {
	return mime.NormalizeMimeType(d.codec.Load().(webrtc.RTPCodecCapability).MimeType)
}

// StreamID is the group this track belongs too. This must be unique
func (d *DownTrack) StreamID() string { return d.params.StreamID }

func (d *DownTrack) SubscriberID() livekit.ParticipantID {
	// add `createdAt` to ensure repeated subscriptions from same subscriber to same publisher does not collide
	return livekit.ParticipantID(fmt.Sprintf("%s:%d", d.params.SubID, d.createdAt))
}

func (d *DownTrack) Receiver() TrackReceiver {
	d.receiverLock.RLock()
	defer d.receiverLock.RUnlock()
	return d.receiver
}

func (d *DownTrack) SetReceiver(r TrackReceiver) {
	d.params.Logger.Debugw("downtrack set receiver", "codec", r.Codec())
	d.bindLock.Lock()
	if d.IsClosed() {
		d.bindLock.Unlock()
		return
	}

	d.receiverLock.Lock()
	old := d.receiver
	d.receiver = r
	d.receiverLock.Unlock()

	old.DeleteDownTrack(d.SubscriberID())
	if err := r.AddDownTrack(d); err != nil {
		d.params.Logger.Warnw("failed to add downtrack to receiver", err)
	}
	d.bindLock.Unlock()

	r.AddOnReady(d.handleReceiverReady)
	d.handleUpstreamCodecChange(r.Codec().MimeType)
	if sal := d.getStreamAllocatorListener(); sal != nil {
		sal.OnSubscribedLayerChanged(d, d.forwarder.MaxLayer())
	}
}

// Sets RTP header extensions for this track
func (d *DownTrack) setRTPHeaderExtensions() {
	sal := d.getStreamAllocatorListener()
	if sal == nil {
		return
	}
	isBWEEnabled := sal.IsBWEEnabled(d)
	bweType := sal.BWEType()

	tr := d.transceiver.Load()
	if tr == nil {
		return
	}
	var extensions []webrtc.RTPHeaderExtensionParameter
	if sender := tr.Sender(); sender != nil {
		extensions = sender.GetParameters().HeaderExtensions
		d.params.Logger.Debugw("negotiated downtrack extensions", "extensions", extensions)
	}

	d.bindLock.Lock()
	for _, ext := range extensions {
		switch ext.URI {
		case sdp.ABSSendTimeURI:
			if isBWEEnabled && bweType == bwe.BWETypeRemote {
				d.absSendTimeExtID = ext.ID
			} else {
				d.absSendTimeExtID = 0
			}
		case dd.ExtensionURI:
			d.dependencyDescriptorExtID = ext.ID
		case pd.PlayoutDelayURI:
			d.playoutDelayExtID = ext.ID
		case sdp.TransportCCURI:
			if isBWEEnabled && bweType == bwe.BWETypeSendSide {
				d.transportWideExtID = ext.ID
			} else {
				d.transportWideExtID = 0
			}
		case act.AbsCaptureTimeURI:
			d.absCaptureTimeExtID = ext.ID
		}
	}
	d.bindLock.Unlock()
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

func (d *DownTrack) SSRCRTX() uint32 {
	return d.ssrcRTX
}

func (d *DownTrack) SetTransceiver(transceiver *webrtc.RTPTransceiver) {
	d.transceiver.Store(transceiver)
	d.setRTPHeaderExtensions()
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
			d.Receiver().SendPLI(layer, false)
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

		d.params.Logger.Debugw(
			"notifying max subscribed layer",
			"layer", maxLayerSpatial,
			"event", event,
		)
		d.params.Listener.OnMaxSubscribedLayerChanged(maxLayerSpatial)
	}

	d.params.Logger.Debugw(
		"notifying max subscribed layer",
		"layer", buffer.InvalidLayerSpatial,
		"event", "close",
	)
	d.params.Listener.OnMaxSubscribedLayerChanged(buffer.InvalidLayerSpatial)
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
		d.params.Logger.Errorw(
			"payload overflow", nil,
			"want", len(extPkt.Packet.Payload[tp.incomingHeaderSize:]),
			"have", n,
		)
		PacketFactory.Put(poolEntity)
		return ErrPayloadOverflow
	}
	payload = payload[:len(tp.codecBytes)+n]

	// translate RTP header
	hdr := &rtp.Header{
		Version:        extPkt.Packet.Version,
		Padding:        extPkt.Packet.Padding,
		PayloadType:    d.getTranslatedPayloadType(extPkt.Packet.PayloadType),
		SequenceNumber: uint16(tp.rtp.extSequenceNumber),
		Timestamp:      uint32(tp.rtp.extTimestamp),
		SSRC:           d.ssrc,
	}
	if tp.marker {
		hdr.Marker = tp.marker
	}

	// add extensions
	if d.dependencyDescriptorExtID != 0 && tp.ddBytes != nil {
		hdr.SetExtension(uint8(d.dependencyDescriptorExtID), tp.ddBytes)
	}
	if d.playoutDelayExtID != 0 && d.playoutDelay != nil {
		if val := d.playoutDelay.GetDelayExtension(hdr.SequenceNumber); val != nil {
			hdr.SetExtension(uint8(d.playoutDelayExtID), val)

			// NOTE: play out delay extension is not cached in sequencer,
			// i. e. they will not be added to retransmitted packet.
			// But, it is okay as the extension is added till a RTCP Receiver Report for
			// the corresponding sequence number is received.
			// The extreme case is all packets containing the play out delay are lost and
			// all of them retransmitted and an RTCP Receiver Report received for those
			// retransmitted sequence numbers. But, that is highly improbable, if not impossible.
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
		_, _, _, refSenderReport := d.forwarder.GetSenderReportParams()
		if refSenderReport != nil {
			actExtCopy := *extPkt.AbsCaptureTimeExt
			if err = actExtCopy.Rewrite(
				rtpstats.RTCPSenderReportPropagationDelay(
					refSenderReport,
					!d.params.DisableSenderReportPassThrough,
				),
			); err == nil {
				actBytes, err = actExtCopy.Marshal()
				if err == nil {
					hdr.SetExtension(uint8(d.absCaptureTimeExtID), actBytes)
				}
			}
		}
	}
	d.addDummyExtensions(hdr)

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

	headerSize := hdr.MarshalSize()
	d.rtpStats.Update(
		extPkt.Arrival,
		tp.rtp.extSequenceNumber,
		tp.rtp.extTimestamp,
		hdr.Marker,
		headerSize,
		len(payload),
		0,
		extPkt.IsOutOfOrder,
	)
	d.pacer.Enqueue(&pacer.Packet{
		Header:             hdr,
		HeaderSize:         headerSize,
		Payload:            payload,
		ProbeClusterId:     ccutils.ProbeClusterId(d.probeClusterId.Load()),
		AbsSendTimeExtID:   uint8(d.absSendTimeExtID),
		TransportWideExtID: uint8(d.transportWideExtID),
		WriteStream:        d.writeStream,
		Pool:               PacketFactory,
		PoolEntity:         poolEntity,
	})

	if extPkt.KeyFrame {
		d.isNACKThrottled.Store(false)
		d.rtpStats.UpdateKeyFrame(1)
		d.params.Logger.Debugw(
			"forwarded key frame",
			"layer", layer,
			"rtpsn", tp.rtp.extSequenceNumber,
			"rtpts", tp.rtp.extTimestamp,
		)
	}

	if tp.isSwitching {
		d.postMaxLayerNotifierEvent("switching")
	}

	if tp.isResuming {
		if sal := d.getStreamAllocatorListener(); sal != nil {
			sal.OnResume(d)
		}
	}
	return nil
}

// WritePaddingRTP tries to write as many padding only RTP packets as necessary
// to satisfy given size to the DownTrack
func (d *DownTrack) WritePaddingRTP(bytesToSend int, paddingOnMute bool, forceMarker bool) int {
	if !d.writable.Load() {
		return 0
	}

	if !paddingOnMute {
		if !d.rtpStats.IsActive() {
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
		if d.forwarder.IsMuted() {
			return 0
		}

		// Hold sending padding packets till first RTCP-RR is received for this RTP stream.
		// That is definitive proof that the remote side knows about this RTP stream.
		if d.rtpStats.LastReceiverReportTime() == 0 {
			return 0
		}
	}

	// RTP padding maximum is 255 bytes. Break it up.
	// Use 20 byte as estimate of RTP header size (12 byte header + 8 byte extension)
	num := (bytesToSend + RTPPaddingMaxPayloadSize + RTPPaddingEstimatedHeaderSize - 1) / (RTPPaddingMaxPayloadSize + RTPPaddingEstimatedHeaderSize)
	if num == 0 {
		return 0
	}

	frameRate := uint32(0)
	if paddingOnMute {
		// advance timestamps when sending dummy padding packets to start a stream
		// to ensure receiver sees proper timestamp and starts the stream
		frameRate = uint32(time.Second / paddingOnMuteInterval)
	}

	snts, err := d.forwarder.GetSnTsForPadding(num, frameRate, forceMarker)
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
	payloads := make([]byte, RTPPaddingMaxPayloadSize*len(snts))
	for i := 0; i < len(snts); i++ {
		hdr := &rtp.Header{
			Version:        2,
			Padding:        true,
			Marker:         false,
			PayloadType:    uint8(d.payloadType.Load()),
			SequenceNumber: uint16(snts[i].extSequenceNumber),
			Timestamp:      uint32(snts[i].extTimestamp),
			SSRC:           d.ssrc,
		}
		d.addDummyExtensions(hdr)

		payload := payloads[i*RTPPaddingMaxPayloadSize : (i+1)*RTPPaddingMaxPayloadSize : (i+1)*RTPPaddingMaxPayloadSize]
		// last byte of padding has padding size including that byte
		payload[RTPPaddingMaxPayloadSize-1] = byte(RTPPaddingMaxPayloadSize)

		hdrSize := hdr.MarshalSize()
		payloadSize := len(payload)
		d.rtpStats.Update(
			mono.UnixNano(),
			snts[i].extSequenceNumber,
			snts[i].extTimestamp,
			hdr.Marker,
			hdrSize,
			0,
			payloadSize,
			false,
		)

		d.pacer.Enqueue(&pacer.Packet{
			Header:             hdr,
			HeaderSize:         hdrSize,
			Payload:            payload,
			ProbeClusterId:     ccutils.ProbeClusterId(d.probeClusterId.Load()),
			IsProbe:            true,
			AbsSendTimeExtID:   uint8(d.absSendTimeExtID),
			TransportWideExtID: uint8(d.transportWideExtID),
			WriteStream:        d.writeStream,
		})

		bytesSent += hdrSize + payloadSize
	}

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
	d.bindLock.Lock()
	if d.isClosed.Swap(true) {
		// already closed
		d.bindLock.Unlock()
		return
	}

	d.params.Logger.Debugw("close down track", "flushBlankFrame", flush)
	if d.bindState.Load() == bindStateBound {
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

		d.params.Logger.Debugw("closing sender", "kind", d.kind)
	}

	d.setBindStateLocked(bindStateUnbound)
	d.Receiver().DeleteDownTrack(d.SubscriberID())

	if d.rtcpReader != nil && flush {
		d.params.Logger.Debugw("downtrack close rtcp reader")
		d.rtcpReader.Close()
		d.rtcpReader.OnPacket(nil)
	}
	if d.rtcpReaderRTX != nil && flush {
		d.params.Logger.Debugw("downtrack close rtcp rtx reader")
		d.rtcpReaderRTX.Close()
		d.rtcpReaderRTX.OnPacket(nil)
	}
	d.bindLock.Unlock()

	d.connectionStats.Close()

	d.rtpStats.Stop()
	d.rtpStatsRTX.Stop()
	d.params.Logger.Debugw("rtp stats",
		"direction", "downstream",
		"mime", d.Mime().String(),
		"ssrc", d.ssrc,
		"stats", d.rtpStats,
		"statsRTX", d.rtpStatsRTX,
	)

	d.maxLayerNotifierChMu.Lock()
	d.maxLayerNotifierChClosed = true
	close(d.maxLayerNotifierCh)
	d.maxLayerNotifierChMu.Unlock()

	d.keyFrameRequesterChMu.Lock()
	d.keyFrameRequesterChClosed = true
	close(d.keyFrameRequesterCh)
	d.keyFrameRequesterChMu.Unlock()

	d.params.Listener.OnDownTrackClose(!flush)
}

func (d *DownTrack) SetMaxSpatialLayer(spatialLayer int32) {
	changed, maxLayer := d.forwarder.SetMaxSpatialLayer(spatialLayer)
	if !changed {
		return
	}

	d.postMaxLayerNotifierEvent("max-subscribed")
	d.postKeyFrameRequestEvent()

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
		RTPStats:                      d.rtpStats,
		DeltaStatsSenderSnapshotId:    d.deltaStatsSenderSnapshotId,
		RTPStatsRTX:                   d.rtpStatsRTX,
		DeltaStatsRTXSenderSnapshotId: d.deltaStatsRTXSenderSnapshotId,
		ForwarderState:                d.forwarder.GetState(),
	}

	if d.playoutDelay != nil {
		dts.PlayoutDelayControllerState = d.playoutDelay.GetState()
	}
	return dts
}

func (d *DownTrack) SeedState(state DownTrackState) {
	if d.writable.Load() {
		return
	}

	if state.RTPStats != nil || state.ForwarderState != nil {
		d.params.Logger.Debugw("seeding down track state", "state", state)
	}
	if state.RTPStats != nil {
		d.rtpStats.Seed(state.RTPStats)
		d.deltaStatsSenderSnapshotId = state.DeltaStatsSenderSnapshotId
		if d.playoutDelay != nil {
			d.playoutDelay.SeedState(state.PlayoutDelayControllerState)
		}
	}
	if state.RTPStatsRTX != nil {
		d.rtpStatsRTX.Seed(state.RTPStatsRTX)
		d.deltaStatsRTXSenderSnapshotId = state.DeltaStatsRTXSenderSnapshotId

		d.rtxSequenceNumber.Store(d.rtpStatsRTX.ExtHighestSequenceNumber())
	}
	d.forwarder.SeedState(state.ForwarderState)
}

func (d *DownTrack) StopWriteAndGetState() DownTrackState {
	d.params.Logger.Debugw("stopping write")
	d.bindLock.Lock()
	d.writable.Store(false)
	d.writeStopped.Store(true)
	d.bindLock.Unlock()

	return d.GetState()
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

func (d *DownTrack) IsDeficient() bool {
	return d.forwarder.IsDeficient()
}

func (d *DownTrack) BandwidthRequested() int64 {
	_, brs := d.Receiver().GetLayeredBitrate()
	return d.forwarder.BandwidthRequested(brs)
}

func (d *DownTrack) DistanceToDesired() float64 {
	al, brs := d.Receiver().GetLayeredBitrate()
	return d.forwarder.DistanceToDesired(al, brs)
}

func (d *DownTrack) AllocateOptimal(allowOvershoot bool, hold bool) VideoAllocation {
	al, brs := d.Receiver().GetLayeredBitrate()
	allocation := d.forwarder.AllocateOptimal(al, brs, allowOvershoot, hold)
	d.postKeyFrameRequestEvent()
	d.maybeAddTransition(allocation.BandwidthNeeded, allocation.DistanceToDesired, allocation.PauseReason)
	return allocation
}

func (d *DownTrack) ProvisionalAllocatePrepare() {
	al, brs := d.Receiver().GetLayeredBitrate()
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
	al, brs := d.Receiver().GetLayeredBitrate()
	allocation, available := d.forwarder.AllocateNextHigher(availableChannelCapacity, al, brs, allowOvershoot)
	d.postKeyFrameRequestEvent()
	d.maybeAddTransition(allocation.BandwidthNeeded, allocation.DistanceToDesired, allocation.PauseReason)
	return allocation, available
}

func (d *DownTrack) GetNextHigherTransition(allowOvershoot bool) (VideoTransition, bool) {
	availableLayers, brs := d.Receiver().GetLayeredBitrate()
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
	al, brs := d.Receiver().GetLayeredBitrate()
	allocation := d.forwarder.Pause(al, brs)
	d.maybeAddTransition(allocation.BandwidthNeeded, allocation.DistanceToDesired, allocation.PauseReason)
	return allocation
}

func (d *DownTrack) Resync() {
	d.forwarder.Resync()
}

func (d *DownTrack) CreateSourceDescriptionChunks() []rtcp.SourceDescriptionChunk {
	transceiver := d.transceiver.Load()
	if d.bindState.Load() != bindStateBound || transceiver == nil {
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
	if d.bindState.Load() != bindStateBound {
		return nil
	}

	_, _, tsOffset, refSenderReport := d.forwarder.GetSenderReportParams()
	return d.rtpStats.GetRtcpSenderReport(d.ssrc, refSenderReport, tsOffset, !d.params.DisableSenderReportPassThrough)

	// not sending RTCP Sender Report for RTX
}

func (d *DownTrack) writeBlankFrameRTP(duration float32, generation uint32) chan struct{} {
	done := make(chan struct{})
	go func() {
		// don't send if not writable OR nothing has been sent
		if !d.writable.Load() || !d.rtpStats.IsActive() {
			close(done)
			return
		}

		mimeType := d.Mime()
		var getBlankFrame func(bool) ([]byte, error)
		switch mimeType {
		case mime.MimeTypeOpus:
			getBlankFrame = d.getOpusBlankFrame
		case mime.MimeTypeRED:
			getBlankFrame = d.getOpusRedBlankFrame
		case mime.MimeTypeVP8:
			getBlankFrame = d.getVP8BlankFrame
		case mime.MimeTypeH264:
			getBlankFrame = d.getH264BlankFrame
		default:
			close(done)
			return
		}

		frameRate := uint32(30)
		if mimeType == mime.MimeTypeOpus || mimeType == mime.MimeTypeRED {
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
				hdr := &rtp.Header{
					Version:        2,
					Padding:        false,
					Marker:         true,
					PayloadType:    uint8(d.payloadType.Load()),
					SequenceNumber: uint16(snts[i].extSequenceNumber),
					Timestamp:      uint32(snts[i].extTimestamp),
					SSRC:           d.ssrc,
				}
				d.addDummyExtensions(hdr)

				payload, err := getBlankFrame(frameEndNeeded)
				if err != nil {
					d.params.Logger.Warnw("could not get blank frame", err)
					close(done)
					return
				}

				headerSize := hdr.MarshalSize()
				d.rtpStats.Update(
					mono.UnixNano(),
					snts[i].extSequenceNumber,
					snts[i].extTimestamp,
					hdr.Marker,
					headerSize,
					len(payload),
					0,
					false,
				)
				d.pacer.Enqueue(&pacer.Packet{
					Header:             hdr,
					HeaderSize:         headerSize,
					Payload:            payload,
					ProbeClusterId:     ccutils.ProbeClusterId(d.probeClusterId.Load()),
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
		d.params.Logger.Errorw("could not unmarshal rtcp receiver packet", err)
		return
	}

	pliOnce := true
	sendPliOnce := func() {
		_, layer := d.forwarder.CheckSync()
		if pliOnce {
			if layer != buffer.InvalidLayerSpatial {
				d.params.Logger.Debugw("sending PLI RTCP", "layer", layer)
				d.Receiver().SendPLI(layer, false)
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

				rtt, isRttChanged := d.rtpStats.UpdateFromReceiverReport(r)
				if isRttChanged {
					rttToReport = rtt
				}

				if d.playoutDelay != nil {
					d.playoutDelay.OnSeqAcked(uint16(r.LastSequenceNumber))
					// screen share track has inaccuracy jitter due to its low frame rate and bursty traffic
					if d.params.Source != livekit.TrackSource_SCREEN_SHARE {
						jitterMs := uint64(r.Jitter*1e3) / uint64(d.clockRate)
						d.playoutDelay.SetJitter(uint32(jitterMs))
					}
				}
			}
			// RTX-TODO: This is used for media loss proxying only as of 2024-12-15.
			// Ideally, this should keep deltas between previous RTCP Receiver Report
			// and current report, calculate the loss in the window and reconcile it with
			// data in a similar window from RTX stream (to ensure losses are discounted
			// for NACKs), but keeping this simple for several reasons
			//   - media loss proxying is a configurable setting and could be disabled
			//   - media loss proxying is used for audio only and audio may not have NACKing
			//   - to keep it simple
			if len(rr.Reports) > 0 {
				d.listenerLock.RLock()
				rrListeners := d.receiverReportListeners
				d.listenerLock.RUnlock()
				for _, l := range rrListeners {
					l(d, rr)
				}
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

		d.params.Listener.OnRttUpdate(rttToReport)
	}
}

func (d *DownTrack) handleRTCPRTX(bytes []byte) {
	pkts, err := rtcp.Unmarshal(bytes)
	if err != nil {
		d.params.Logger.Errorw("could not unmarshal rtcp rtx receiver packet", err)
		return
	}

	for _, pkt := range pkts {
		switch p := pkt.(type) {
		case *rtcp.ReceiverReport:
			for _, r := range p.Reports {
				if r.SSRC != d.ssrcRTX {
					continue
				}

				d.rtpStatsRTX.UpdateFromReceiverReport(r)
			}

		case *rtcp.ReceiverEstimatedMaximumBitrate:
			if sal := d.getStreamAllocatorListener(); sal != nil {
				sal.OnREMB(d, p)
			}

		case *rtcp.TransportLayerCC:
			if p.MediaSSRC == d.ssrcRTX {
				if sal := d.getStreamAllocatorListener(); sal != nil {
					sal.OnTransportCCFeedback(d, p)
				}
			}
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

func (d *DownTrack) retransmitPacket(epm *extPacketMeta, sourcePkt []byte, isProbe bool) (int, error) {
	var pkt rtp.Packet
	if err := pkt.Unmarshal(sourcePkt); err != nil {
		d.params.Logger.Errorw("could not unmarshal rtp packet to send via RTX", err)
		return 0, err
	}
	hdr := &rtp.Header{
		Version:        pkt.Header.Version,
		Padding:        pkt.Header.Padding,
		Marker:         epm.marker,
		PayloadType:    d.getTranslatedPayloadType(pkt.Header.PayloadType),
		SequenceNumber: epm.targetSeqNo,
		Timestamp:      epm.timestamp,
		SSRC:           d.ssrc,
	}
	rtxOffset := 0
	var rtxExtSequenceNumber uint64
	if rtxPT := d.payloadTypeRTX.Load(); rtxPT != 0 && d.ssrcRTX != 0 {
		rtxExtSequenceNumber = d.rtxSequenceNumber.Inc()
		rtxOffset = 2

		hdr.PayloadType = uint8(rtxPT)
		hdr.SequenceNumber = uint16(rtxExtSequenceNumber)
		hdr.SSRC = d.ssrcRTX
	}

	if d.dependencyDescriptorExtID != 0 {
		var ddBytes []byte
		if len(epm.ddBytesSlice) != 0 {
			ddBytes = epm.ddBytesSlice
		} else {
			ddBytes = epm.ddBytes[:epm.ddBytesSize]
		}
		if len(ddBytes) != 0 {
			hdr.SetExtension(uint8(d.dependencyDescriptorExtID), ddBytes)
		}
	}
	if d.absCaptureTimeExtID != 0 && len(epm.actBytes) != 0 {
		hdr.SetExtension(uint8(d.absCaptureTimeExtID), epm.actBytes)
	}
	d.addDummyExtensions(hdr)

	poolEntity := PacketFactory.Get().(*[]byte)
	payload := *poolEntity
	if rtxOffset != 0 {
		// write OSN (Original Sequence Number)
		binary.BigEndian.PutUint16(payload[0:2], epm.targetSeqNo)
	}
	if len(epm.codecBytesSlice) != 0 {
		n := copy(payload[rtxOffset:], epm.codecBytesSlice)
		m := copy(payload[rtxOffset+n:], pkt.Payload[epm.numCodecBytesIn:])
		payload = payload[:rtxOffset+n+m]
	} else {
		copy(payload[rtxOffset:], epm.codecBytes[:epm.numCodecBytesOut])
		copy(payload[rtxOffset+int(epm.numCodecBytesOut):], pkt.Payload[epm.numCodecBytesIn:])
		payload = payload[:rtxOffset+int(epm.numCodecBytesOut)+len(pkt.Payload)-int(epm.numCodecBytesIn)]
	}

	headerSize := hdr.MarshalSize()
	var (
		payloadSize, paddingSize int
		isOutOfOrder             bool
	)
	if isProbe {
		// although not padding only packets, marking it as padding for accounting as padding is used to signify probing,
		// also not marking them as out-of-order although sequence numbers in packets are out-of-order because of re-sending packets
		payloadSize, paddingSize, isOutOfOrder = 0, len(payload), false
	} else {
		payloadSize, paddingSize, isOutOfOrder = len(payload), 0, true
	}
	if hdr.SSRC == d.ssrcRTX {
		d.rtpStatsRTX.Update(
			mono.UnixNano(),
			rtxExtSequenceNumber,
			0,
			hdr.Marker,
			headerSize,
			payloadSize,
			paddingSize,
			isOutOfOrder,
		)
	} else {
		d.rtpStats.Update(
			mono.UnixNano(),
			epm.extSequenceNumber,
			epm.extTimestamp,
			hdr.Marker,
			headerSize,
			payloadSize,
			paddingSize,
			isOutOfOrder,
		)
	}
	d.pacer.Enqueue(&pacer.Packet{
		Header:             hdr,
		HeaderSize:         headerSize,
		Payload:            payload,
		ProbeClusterId:     ccutils.ProbeClusterId(d.probeClusterId.Load()),
		IsProbe:            isProbe,
		IsRTX:              !isProbe,
		AbsSendTimeExtID:   uint8(d.absSendTimeExtID),
		TransportWideExtID: uint8(d.transportWideExtID),
		WriteStream:        d.writeStream,
		Pool:               PacketFactory,
		PoolEntity:         poolEntity,
	})
	return headerSize + len(payload), nil
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

	receiver := d.Receiver()

	nackAcks := uint32(0)
	nackMisses := uint32(0)
	numRepeatedNACKs := uint32(0)
	for _, epm := range d.sequencer.getExtPacketMetas(filtered) {
		if disallowedLayers[epm.layer] {
			continue
		}

		nackAcks++

		pktBuff := *src
		n, err := receiver.ReadRTP(pktBuff, uint8(epm.layer), epm.sourceSeqNo)
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

		d.retransmitPacket(&epm, pktBuff[:n], false)
	}

	d.totalRepeatedNACKs.Add(numRepeatedNACKs)

	d.rtpStats.UpdateNackProcessed(nackAcks, nackMisses, numRepeatedNACKs)
}

func (d *DownTrack) WriteProbePackets(bytesToSend int, usePadding bool) int {
	rtxPT := uint8(d.payloadTypeRTX.Load())
	if rtxPT == 0 || d.ssrcRTX == 0 {
		return d.WritePaddingRTP(bytesToSend, false, false)
	}

	if !d.writable.Load() ||
		!d.rtpStats.IsActive() ||
		(d.absSendTimeExtID == 0 && d.transportWideExtID == 0) ||
		d.rtpStats.LastReceiverReportTime() == 0 ||
		d.sequencer == nil {
		return 0
	}

	bytesSent := 0

	if usePadding {
		num := (bytesToSend + RTPPaddingMaxPayloadSize + RTPPaddingEstimatedHeaderSize - 1) / (RTPPaddingMaxPayloadSize + RTPPaddingEstimatedHeaderSize)
		if num == 0 {
			return 0
		}

		payloads := make([]byte, RTPPaddingMaxPayloadSize*num)
		for i := 0; i < num; i++ {
			rtxExtSequenceNumber := d.rtxSequenceNumber.Inc()
			hdr := &rtp.Header{
				Version:        2,
				Padding:        true,
				Marker:         false,
				PayloadType:    rtxPT,
				SequenceNumber: uint16(rtxExtSequenceNumber),
				Timestamp:      0,
				SSRC:           d.ssrcRTX,
			}
			d.addDummyExtensions(hdr)

			payload := payloads[i*RTPPaddingMaxPayloadSize : (i+1)*RTPPaddingMaxPayloadSize : (i+1)*RTPPaddingMaxPayloadSize]
			// last byte of padding has padding size including that byte
			payload[RTPPaddingMaxPayloadSize-1] = byte(RTPPaddingMaxPayloadSize)

			hdrSize := hdr.MarshalSize()
			payloadSize := len(payload)
			d.rtpStatsRTX.Update(
				mono.UnixNano(),
				rtxExtSequenceNumber,
				0,
				hdr.Marker,
				hdrSize,
				0,
				payloadSize,
				false,
			)
			d.pacer.Enqueue(&pacer.Packet{
				Header:             hdr,
				HeaderSize:         hdrSize,
				Payload:            payload,
				ProbeClusterId:     ccutils.ProbeClusterId(d.probeClusterId.Load()),
				IsProbe:            true,
				AbsSendTimeExtID:   uint8(d.absSendTimeExtID),
				TransportWideExtID: uint8(d.transportWideExtID),
				WriteStream:        d.writeStream,
			})

			bytesSent += hdrSize + payloadSize
		}
	} else {
		src := PacketFactory.Get().(*[]byte)
		defer PacketFactory.Put(src)

		receiver := d.Receiver()

		endExtHighestSequenceNumber := d.rtpStats.ExtHighestSequenceNumber()
		startExtHighestSequenceNumber := endExtHighestSequenceNumber - 5
		for esn := startExtHighestSequenceNumber; esn <= endExtHighestSequenceNumber; esn++ {
			epm := d.sequencer.lookupExtPacketMeta(esn)
			if epm == nil {
				continue
			}

			pktBuff := *src
			n, err := receiver.ReadRTP(pktBuff, uint8(epm.layer), epm.sourceSeqNo)
			if err != nil {
				if err == io.EOF {
					break
				}
				continue
			}

			sent, _ := d.retransmitPacket(epm, pktBuff[:n], true)
			bytesSent += sent
			if bytesSent >= bytesToSend {
				break
			}
		}
	}

	return bytesSent
}

func (d *DownTrack) addDummyExtensions(hdr *rtp.Header) {
	// add dummy extensions (actual ones will be filed by pacer) to get header size
	if d.absSendTimeExtID != 0 {
		hdr.SetExtension(uint8(d.absSendTimeExtID), dummyAbsSendTimeExt)
	}
	if d.transportWideExtID != 0 {
		hdr.SetExtension(uint8(d.transportWideExtID), dummyTransportCCExt)
	}
}

func (d *DownTrack) getTranslatedPayloadType(srcPT uint8) uint8 {
	// send primary codec to subscriber if the publisher sent primary codec when red is negotiated,
	// this will happen when the payload is too large to encode into red payload (exceeds mtu).
	if d.isRED && srcPT == d.upstreamPrimaryPT && d.primaryPT != 0 {
		return d.primaryPT
	}
	return uint8(d.payloadType.Load())
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
		"MimeType":            d.Mime().String(),
		"BindState":           d.bindState.Load().(bindState),
		"Muted":               d.forwarder.IsMuted(),
		"PubMuted":            d.forwarder.IsPubMuted(),
		"CurrentSpatialLayer": d.forwarder.CurrentLayer().Spatial,
		"Stats":               stats,
	}
}

func (d *DownTrack) GetConnectionScoreAndQuality() (float32, livekit.ConnectionQuality) {
	return d.connectionStats.GetScoreAndQuality()
}

func (d *DownTrack) GetTrackStats() *livekit.RTPStats {
	return rtpstats.ReconcileRTPStatsWithRTX(d.rtpStats.ToProto(), d.rtpStatsRTX.ToProto())
}

func (d *DownTrack) deltaStats(ds *rtpstats.RTPDeltaInfo, dsrv *rtpstats.RTPDeltaInfo) map[uint32]*buffer.StreamStatsWithLayers {
	if ds == nil && dsrv == nil {
		return nil
	}

	streamStats := make(map[uint32]*buffer.StreamStatsWithLayers, 1)
	streamStats[d.ssrc] = &buffer.StreamStatsWithLayers{
		RTPStats:           ds,
		RTPStatsRemoteView: dsrv,
		Layers: map[int32]*rtpstats.RTPDeltaInfo{
			0: ds,
		},
	}

	return streamStats
}

func (d *DownTrack) GetDeltaStatsSender() map[uint32]*buffer.StreamStatsWithLayers {
	ds, dsrv := d.rtpStats.DeltaInfoSender(d.deltaStatsSenderSnapshotId)
	dsRTX, dsrvRTX := d.rtpStatsRTX.DeltaInfoSender(d.deltaStatsRTXSenderSnapshotId)
	return d.deltaStats(
		rtpstats.ReconcileRTPDeltaInfoWithRTX(ds, dsRTX),
		rtpstats.ReconcileRTPDeltaInfoWithRTX(dsrv, dsrvRTX),
	)
}

func (d *DownTrack) GetPrimaryStreamLastReceiverReportTime() time.Time {
	return time.Unix(0, d.rtpStats.LastReceiverReportTime())
}

func (d *DownTrack) GetPrimaryStreamPacketsSent() uint64 {
	return d.rtpStats.GetPacketsSeenMinusPadding()
}

func (d *DownTrack) GetNackStats() (totalPackets uint32, totalRepeatedNACKs uint32) {
	totalPackets = uint32(d.rtpStats.GetPacketsSeenMinusPadding())
	totalRepeatedNACKs = d.totalRepeatedNACKs.Load()
	return
}

func (d *DownTrack) onBindAndConnectedChange() {
	if d.writeStopped.Load() {
		return
	}
	d.writable.Store(d.connected.Load() && d.bindState.Load() == bindStateBound)
	if d.connected.Load() && d.bindState.Load() == bindStateBound && !d.bindAndConnectedOnce.Swap(true) {
		go d.params.Listener.OnBindAndConnected()

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
	} else if d.Mime() == mime.MimeTypeOpus {
		d.sendSilentFrameOnMuteForOpus()
	}
}

func (d *DownTrack) sendPaddingOnMuteForVideo() {
	numPackets := maxPaddingOnMuteDuration / paddingOnMuteInterval
	for i := range int(numPackets) {
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
		for i := range len(snts) {
			hdr := &rtp.Header{
				Version:        2,
				Padding:        false,
				Marker:         true,
				PayloadType:    uint8(d.payloadType.Load()),
				SequenceNumber: uint16(snts[i].extSequenceNumber),
				Timestamp:      uint32(snts[i].extTimestamp),
				SSRC:           d.ssrc,
			}
			d.addDummyExtensions(hdr)

			payload, err := d.getOpusBlankFrame(false)
			if err != nil {
				d.params.Logger.Warnw("could not get blank frame", err)
				return
			}

			headerSize := hdr.MarshalSize()
			d.rtpStats.Update(
				mono.UnixNano(),
				snts[i].extSequenceNumber,
				snts[i].extTimestamp,
				hdr.Marker,
				headerSize,
				0,
				len(payload), // although this is using empty frames, mark as padding as these are used to trigger Pion OnTrack only
				false,
			)
			d.pacer.Enqueue(&pacer.Packet{
				Header:             hdr,
				HeaderSize:         headerSize,
				Payload:            payload,
				ProbeClusterId:     ccutils.ProbeClusterId(d.probeClusterId.Load()),
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
	layer int32,
	publisherSRData *livekit.RTCPSenderReportState,
) error {
	d.forwarder.SetRefSenderReport(layer, publisherSRData)

	currentLayer, isSingleStream, tsOffset, refSenderReport := d.forwarder.GetSenderReportParams()
	if layer == currentLayer || (layer == 0 && isSingleStream) {
		d.handleRTCPSenderReportData(refSenderReport, tsOffset)
	}
	return nil
}

func (d *DownTrack) handleRTCPSenderReportData(publisherSRData *livekit.RTCPSenderReportState, tsOffset uint64) {
	d.rtpStats.MaybeAdjustFirstPacketTime(publisherSRData, tsOffset)
}

// -------------------------------------------------------------------------------
