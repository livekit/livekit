package sfu

import (
	"io"
	"math/rand"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"
	"github.com/rs/zerolog/log"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
)

type AudioLevelHandle func(level uint8, duration uint32)
type Bitrates [DefaultMaxLayerSpatial + 1][DefaultMaxLayerTemporal + 1]int64

// TrackReceiver defines an interface receive media from remote peer
type TrackReceiver interface {
	TrackID() livekit.TrackID
	StreamID() string
	Codec() webrtc.RTPCodecCapability

	ReadRTP(buf []byte, layer uint8, sn uint16) (int, error)
	GetSenderReportTime(layer int32) (rtpTS uint32, ntpTS uint64)
	GetBitrateTemporalCumulative() Bitrates

	SendPLI(layer int32)

	SetUpTrackPaused(paused bool)
	SetMaxExpectedSpatialLayer(layer int32)

	AddDownTrack(track TrackSender)
	DeleteDownTrack(peerID livekit.ParticipantID)

	DebugInfo() map[string]interface{}
}

// WebRTCReceiver receives a media track
type WebRTCReceiver struct {
	logger logger.Logger

	peerID         livekit.ParticipantID
	trackID        livekit.TrackID
	streamID       string
	kind           webrtc.RTPCodecType
	receiver       *webrtc.RTPReceiver
	codec          webrtc.RTPCodecParameters
	isSimulcast    bool
	onCloseHandler func()
	closeOnce      sync.Once
	closed         atomicBool
	useTrackers    bool

	rtcpMu      sync.Mutex
	rtcpCh      chan []rtcp.Packet
	lastPli     atomicInt64
	pliThrottle int64

	bufferMu sync.RWMutex
	buffers  [DefaultMaxLayerSpatial + 1]*buffer.Buffer

	upTrackMu sync.RWMutex
	upTracks  [DefaultMaxLayerSpatial + 1]*webrtc.TrackRemote

	downTrackMu sync.RWMutex
	downTracks  []TrackSender
	index       map[livekit.ParticipantID]int
	free        map[int]struct{}
	numProcs    int
	lbThreshold int

	streamTrackerManager *StreamTrackerManager
}

func RidToLayer(rid string) int32 {
	switch rid {
	case FullResolution:
		return 2
	case HalfResolution:
		return 1
	default:
		return 0
	}
}

type ReceiverOpts func(w *WebRTCReceiver) *WebRTCReceiver

// WithPliThrottle indicates minimum time(ms) between sending PLIs
func WithPliThrottle(period int64) ReceiverOpts {
	return func(w *WebRTCReceiver) *WebRTCReceiver {
		w.pliThrottle = period * 1e6
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
	pid livekit.ParticipantID,
	logger logger.Logger,
	opts ...ReceiverOpts,
) *WebRTCReceiver {
	w := &WebRTCReceiver{
		logger:   logger,
		peerID:   pid,
		receiver: receiver,
		trackID:  livekit.TrackID(track.ID()),
		streamID: track.StreamID(),
		codec:    track.Codec(),
		kind:     track.Kind(),
		// LK-TODO: this should be based on VideoLayers protocol message rather than RID based
		isSimulcast:          len(track.RID()) > 0,
		pliThrottle:          500e6,
		downTracks:           make([]TrackSender, 0),
		index:                make(map[livekit.ParticipantID]int),
		free:                 make(map[int]struct{}),
		numProcs:             runtime.NumCPU(),
		streamTrackerManager: NewStreamTrackerManager(),
	}
	w.streamTrackerManager.OnAvailableLayersChanged(w.downTrackLayerChange)
	if runtime.GOMAXPROCS(0) < w.numProcs {
		w.numProcs = runtime.GOMAXPROCS(0)
	}
	for _, opt := range opts {
		w = opt(w)
	}
	return w
}

func (w *WebRTCReceiver) SetTrackMeta(trackID livekit.TrackID, streamID string) {
	w.streamID = streamID
	w.trackID = trackID
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

func (w *WebRTCReceiver) Codec() webrtc.RTPCodecCapability {
	return w.codec.RTPCodecCapability
}

func (w *WebRTCReceiver) Kind() webrtc.RTPCodecType {
	return w.kind
}

func (w *WebRTCReceiver) AddUpTrack(track *webrtc.TrackRemote, buff *buffer.Buffer) {
	if w.closed.get() {
		return
	}

	buff.SetLogger(w.logger)

	layer := RidToLayer(track.RID())

	w.upTrackMu.Lock()
	w.upTracks[layer] = track
	w.upTrackMu.Unlock()

	w.bufferMu.Lock()
	w.buffers[layer] = buff
	w.bufferMu.Unlock()

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

func (w *WebRTCReceiver) AddDownTrack(track TrackSender) {
	if w.closed.get() {
		return
	}

	w.downTrackMu.RLock()
	_, ok := w.index[track.PeerID()]
	w.downTrackMu.RUnlock()
	if ok {
		return
	}

	if w.Kind() == webrtc.RTPCodecTypeVideo {
		// notify added down track of available layers
		layers := w.streamTrackerManager.GetAvailableLayers()
		if len(layers) != 0 {
			track.UpTrackLayersChange(layers)
		}
	}

	w.storeDownTrack(track)
}

func (w *WebRTCReceiver) SetMaxExpectedSpatialLayer(layer int32) {
	w.streamTrackerManager.SetMaxExpectedSpatialLayer(layer)
}

func (w *WebRTCReceiver) NumAvailableSpatialLayers() int {
	return len(w.streamTrackerManager.GetAvailableLayers())
}

func (w *WebRTCReceiver) downTrackLayerChange(layers []int32) {
	w.downTrackMu.RLock()
	downTracks := w.downTracks
	w.downTrackMu.RUnlock()

	for _, dt := range downTracks {
		if dt != nil {
			dt.UpTrackLayersChange(layers)
		}
	}
}

func (w *WebRTCReceiver) GetBitrateTemporalCumulative() Bitrates {
	// LK-TODO: For SVC tracks, need to accumulate across spatial layers also
	var br Bitrates
	w.bufferMu.RLock()
	defer w.bufferMu.RUnlock()
	for i, buff := range w.buffers {
		if buff != nil {
			tls := make([]int64, DefaultMaxLayerTemporal+1)
			if w.streamTrackerManager.HasSpatialLayer(int32(i)) {
				tls = buff.BitrateTemporalCumulative()
			}

			for j := 0; j < len(br[i]); j++ {
				br[i][j] = tls[j]
			}
		}
	}
	return br
}

// OnCloseHandler method to be called on remote tracked removed
func (w *WebRTCReceiver) OnCloseHandler(fn func()) {
	w.onCloseHandler = fn
}

// DeleteDownTrack removes a DownTrack from a Receiver
func (w *WebRTCReceiver) DeleteDownTrack(peerID livekit.ParticipantID) {
	if w.closed.get() {
		return
	}

	w.downTrackMu.Lock()
	defer w.downTrackMu.Unlock()

	idx, ok := w.index[peerID]
	if !ok {
		return
	}
	delete(w.index, peerID)
	w.downTracks[idx] = nil
	w.free[idx] = struct{}{}
}

func (w *WebRTCReceiver) SendRTCP(p []rtcp.Packet) {
	if _, ok := p[0].(*rtcp.PictureLossIndication); ok {
		w.rtcpMu.Lock()
		throttled := time.Now().UnixNano()-w.lastPli.get() < w.pliThrottle
		w.rtcpMu.Unlock()
		if throttled {
			return
		}
		w.lastPli.set(time.Now().UnixNano())
	}

	w.rtcpCh <- p
}

func (w *WebRTCReceiver) SendPLI(layer int32) {
	pli := []rtcp.Packet{
		&rtcp.PictureLossIndication{SenderSSRC: rand.Uint32(), MediaSSRC: w.SSRC(int(layer))},
	}

	w.SendRTCP(pli)
}

func (w *WebRTCReceiver) SetRTCPCh(ch chan []rtcp.Packet) {
	w.rtcpCh = ch
}

func (w *WebRTCReceiver) GetSenderReportTime(layer int32) (rtpTS uint32, ntpTS uint64) {
	w.bufferMu.RLock()
	defer w.bufferMu.RUnlock()
	if w.buffers[layer] != nil {
		rtpTS, ntpTS, _ = w.buffers[layer].GetSenderReportData()
	}
	return
}

func (w *WebRTCReceiver) ReadRTP(buf []byte, layer uint8, sn uint16) (int, error) {
	w.bufferMu.RLock()
	buff := w.buffers[layer]
	w.bufferMu.RUnlock()
	return buff.GetPacket(buf, sn)
}

func (w *WebRTCReceiver) GetTotalBytes() uint64 {
	w.bufferMu.RLock()
	defer w.bufferMu.RUnlock()

	totalBytes := uint64(0)
	for _, buff := range w.buffers {
		if buff != nil {
			stats := buff.GetStats()
			totalBytes += stats.TotalBytes
		}
	}

	return totalBytes
}

func (w *WebRTCReceiver) forwardRTP(layer int32) {
	tracker := w.streamTrackerManager.GetTracker(layer)

	defer func() {
		w.closeOnce.Do(func() {
			w.closed.set(true)
			w.closeTracks()
		})

		w.streamTrackerManager.RemoveTracker(layer)
	}()

	for {
		w.bufferMu.RLock()
		buf := w.buffers[layer]
		w.bufferMu.RUnlock()
		pkt, err := buf.ReadExtended()
		if err == io.EOF {
			return
		}

		if tracker != nil {
			tracker.Observe(pkt.Packet.SequenceNumber)
		}

		w.downTrackMu.RLock()
		downTracks := w.downTracks
		free := w.free
		w.downTrackMu.RUnlock()
		if w.lbThreshold == 0 || len(downTracks)-len(free) < w.lbThreshold {
			// serial - not enough down tracks for parallelization to outweigh overhead
			for _, dt := range downTracks {
				if dt != nil {
					w.writeRTP(layer, dt, pkt)
				}
			}
		} else {
			// parallel - enables much more efficient multi-core utilization
			start := uint64(0)
			end := uint64(len(downTracks))

			// 100µs is enough to amortize the overhead and provide sufficient load balancing.
			// WriteRTP takes about 50µs on average, so we write to 2 down tracks per loop.
			step := uint64(2)

			var wg sync.WaitGroup
			wg.Add(w.numProcs)
			for p := 0; p < w.numProcs; p++ {
				go func() {
					defer wg.Done()
					for {
						n := atomic.AddUint64(&start, step)
						if n >= end+step {
							return
						}

						for i := n - step; i < n && i < end; i++ {
							if dt := downTracks[i]; dt != nil {
								w.writeRTP(layer, dt, pkt)
							}
						}
					}
				}()
			}
			wg.Wait()
		}
	}
}

func (w *WebRTCReceiver) writeRTP(layer int32, dt TrackSender, pkt *buffer.ExtPacket) {
	if err := dt.WriteRTP(pkt, layer); err != nil {
		log.Error().Err(err).Str("id", dt.ID()).Msg("Error writing to down track")
	}
}

// closeTracks close all tracks from Receiver
func (w *WebRTCReceiver) closeTracks() {
	w.downTrackMu.Lock()
	for _, dt := range w.downTracks {
		if dt != nil {
			dt.Close()
		}
	}
	w.downTracks = make([]TrackSender, 0)
	w.index = make(map[livekit.ParticipantID]int)
	w.free = make(map[int]struct{})
	w.downTrackMu.Unlock()

	if w.onCloseHandler != nil {
		w.onCloseHandler()
	}
}

func (w *WebRTCReceiver) storeDownTrack(track TrackSender) {
	w.downTrackMu.Lock()
	defer w.downTrackMu.Unlock()

	for idx := range w.free {
		w.index[track.PeerID()] = idx
		w.downTracks[idx] = track
		delete(w.free, idx)
		return
	}

	w.index[track.PeerID()] = len(w.downTracks)
	w.downTracks = append(w.downTracks, track)
}

func (w *WebRTCReceiver) DebugInfo() map[string]interface{} {
	info := map[string]interface{}{
		"Simulcast": w.isSimulcast,
		"LastPli":   w.lastPli,
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
