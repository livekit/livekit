package sfu

import (
	"io"
	"math/rand"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gammazero/workerpool"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
	"github.com/rs/zerolog/log"

	"github.com/pion/ion-sfu/pkg/buffer"
	"github.com/pion/ion-sfu/pkg/stats"
)

// Receiver defines a interface for a track receivers
type Receiver interface {
	TrackID() string
	StreamID() string
	Codec() webrtc.RTPCodecParameters
	Kind() webrtc.RTPCodecType
	SSRC(layer int) uint32
	SetTrackMeta(trackID, streamID string)
	AddUpTrack(track *webrtc.TrackRemote, buffer *buffer.Buffer, bestQualityFirst bool)
	AddDownTrack(track *DownTrack, bestQualityFirst bool)
	SetUpTrackPaused(paused bool)
	HasSpatialLayer(layer int32) bool
	GetBitrate() [3]uint64
	GetBitrateTemporal() [3][4]uint64
	GetBitrateTemporalCumulative() [3][4]uint64
	GetMaxTemporalLayer() [3]int32
	RetransmitPackets(track *DownTrack, packets []packetMeta) error
	DeleteDownTrack(peerID string)
	OnCloseHandler(fn func())
	SendRTCP(p []rtcp.Packet)
	SetRTCPCh(ch chan []rtcp.Packet)

	GetSenderReportTime(layer int32) (rtpTS uint32, ntpTS uint64)
	DebugInfo() map[string]interface{}
}

const (
	lostUpdateDelta = 1e9
)

// WebRTCReceiver receives a video track
type WebRTCReceiver struct {
	peerID          string
	trackID         string
	streamID        string
	kind            webrtc.RTPCodecType
	stream          string
	receiver        *webrtc.RTPReceiver
	codec           webrtc.RTPCodecParameters
	stats           [3]*stats.Stream
	nackWorker      *workerpool.WorkerPool
	isSimulcast     bool
	availableLayers atomic.Value
	onCloseHandler  func()
	closeOnce       sync.Once
	closed          atomicBool
	trackers        [3]*StreamTracker
	useTrackers     bool

	rtcpMu      sync.Mutex
	rtcpCh      chan []rtcp.Packet
	lastPli     atomicInt64
	pliThrottle int64

	bufferMu sync.RWMutex
	buffers  [3]*buffer.Buffer

	upTrackMu sync.RWMutex
	upTracks  [3]*webrtc.TrackRemote

	downTrackMu sync.RWMutex
	downTracks  []*DownTrack
	index       map[string]int
	free        map[int]struct{}
	numProcs    int
	lbThreshold int

	fracLostMu        sync.Mutex
	maxDownFracLost   uint8
	maxDownFracLostTs time.Time
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

// NewWebRTCReceiver creates a new webrtc track receivers
func NewWebRTCReceiver(receiver *webrtc.RTPReceiver, track *webrtc.TrackRemote, pid string, opts ...ReceiverOpts) Receiver {
	w := &WebRTCReceiver{
		peerID:      pid,
		receiver:    receiver,
		trackID:     track.ID(),
		streamID:    track.StreamID(),
		codec:       track.Codec(),
		kind:        track.Kind(),
		nackWorker:  workerpool.New(1),
		isSimulcast: len(track.RID()) > 0,
		pliThrottle: 500e6,
		downTracks:  make([]*DownTrack, 0),
		index:       make(map[string]int),
		free:        make(map[int]struct{}),
		numProcs:    runtime.NumCPU(),
	}
	if runtime.GOMAXPROCS(0) < w.numProcs {
		w.numProcs = runtime.GOMAXPROCS(0)
	}
	for _, opt := range opts {
		w = opt(w)
	}
	return w
}

func (w *WebRTCReceiver) SetTrackMeta(trackID, streamID string) {
	w.streamID = streamID
	w.trackID = trackID
}

func (w *WebRTCReceiver) StreamID() string {
	return w.streamID
}

func (w *WebRTCReceiver) TrackID() string {
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

func (w *WebRTCReceiver) Codec() webrtc.RTPCodecParameters {
	return w.codec
}

func (w *WebRTCReceiver) Kind() webrtc.RTPCodecType {
	return w.kind
}

func (w *WebRTCReceiver) AddUpTrack(track *webrtc.TrackRemote, buff *buffer.Buffer, bestQualityFirst bool) {
	if w.closed.get() {
		return
	}

	var layer int32
	switch track.RID() {
	case fullResolution:
		layer = 2
	case halfResolution:
		layer = 1
	default:
		layer = 0
	}

	w.upTrackMu.Lock()
	w.upTracks[layer] = track
	w.upTrackMu.Unlock()

	w.bufferMu.Lock()
	w.buffers[layer] = buff
	w.bufferMu.Unlock()

	if w.isSimulcast {
		w.addAvailableLayer(uint16(layer), false)

		w.downTrackMu.RLock()
		// LK-TODO-START
		// DownTrack layer change should not happen directly from here.
		// Layer switching should be controlled by StreamAllocator. So, this
		// should call into DownTrack to notify availability of a new layer.
		// One challenge to think about is that the layer bitrate is not available
		// for a second after start up as sfu.Buffer reports at that cadence.
		// One possibility is to initialize sfu.Buffer with default bitrate
		// based on layer.
		// LK-TODO-END
		for _, dt := range w.downTracks {
			if dt != nil {
				if (bestQualityFirst && layer > dt.CurrentSpatialLayer()) ||
					(!bestQualityFirst && layer < dt.CurrentSpatialLayer()) {
					_ = dt.SwitchSpatialLayer(layer, false)
				}
			}
		}
		w.downTrackMu.RUnlock()

		// always publish lowest layer
		if layer != 0 && w.useTrackers {
			tracker := NewStreamTracker()
			w.trackers[layer] = tracker
			tracker.OnStatusChanged = func(status StreamStatus) {
				if status == StreamStatusStopped {
					w.removeAvailableLayer(uint16(layer))
				} else {
					w.addAvailableLayer(uint16(layer), true)
				}
			}
			tracker.Start()
		}
	}
	go w.forwardRTP(layer)
}

// SetUpTrackPaused indicates upstream will not be sending any data.
// this will reflect the "muted" status and will pause streamtracker to ensure we don't turn off
// the layer
func (w *WebRTCReceiver) SetUpTrackPaused(paused bool) {
	if !w.isSimulcast {
		return
	}
	w.upTrackMu.Lock()
	defer w.upTrackMu.Unlock()
	for _, tracker := range w.trackers {
		if tracker != nil {
			tracker.SetPaused(paused)
		}
	}
}

func (w *WebRTCReceiver) AddDownTrack(track *DownTrack, bestQualityFirst bool) {
	if w.closed.get() {
		return
	}

	layer := 0

	w.downTrackMu.RLock()
	_, ok := w.index[track.peerID]
	w.downTrackMu.RUnlock()
	if ok {
		return
	}

	if w.isSimulcast {
		w.upTrackMu.RLock()
		for i, t := range w.upTracks {
			if t != nil && w.HasSpatialLayer(int32(i)) {
				layer = i
				if !bestQualityFirst {
					break
				}
			}
		}
		w.upTrackMu.RUnlock()

		track.SetInitialLayers(int32(layer), 2)
		track.maxSpatialLayer.set(2)
		track.maxTemporalLayer.set(2)
		track.lastSSRC.set(w.SSRC(layer))
		track.trackType = SimulcastDownTrack
		track.payload = packetFactory.Get().(*[]byte)
	} else {
		// LK-TODO-START
		// check if any webrtc client does more than one temporal layer when not simulcasting.
		// Maybe okay to just set the max temporal layer to 2 even in this case.
		// Don't think there is any harm is setting it at 2 if the upper layers are
		// not going to be there.
		// LK-TODO-END
		track.SetInitialLayers(0, 0)
		track.trackType = SimpleDownTrack
	}

	w.storeDownTrack(track)
}

func (w *WebRTCReceiver) HasSpatialLayer(layer int32) bool {
	layers, ok := w.availableLayers.Load().([]uint16)
	if !ok {
		return false
	}
	desired := uint16(layer)
	for _, l := range layers {
		if l == desired {
			return true
		}
	}
	return false
}

func (w *WebRTCReceiver) downtrackLayerChange(layers []uint16, layerAdded bool) {
	w.downTrackMu.RLock()
	defer w.downTrackMu.RUnlock()
	for _, dt := range w.downTracks {
		if dt != nil {
			_, _ = dt.UptrackLayersChange(layers, layerAdded)
		}
	}
}

func (w *WebRTCReceiver) addAvailableLayer(layer uint16, updateDownTrack bool) {
	w.upTrackMu.Lock()
	layers, ok := w.availableLayers.Load().([]uint16)
	if !ok {
		layers = []uint16{}
	}
	hasLayer := false
	for _, l := range layers {
		if l == layer {
			hasLayer = true
			break
		}
	}
	if !hasLayer {
		layers = append(layers, layer)
	}
	w.availableLayers.Store(layers)
	w.upTrackMu.Unlock()

	if updateDownTrack {
		w.downtrackLayerChange(layers, true)
	}
}

func (w *WebRTCReceiver) removeAvailableLayer(layer uint16) {
	w.upTrackMu.Lock()
	layers, ok := w.availableLayers.Load().([]uint16)
	if !ok {
		w.upTrackMu.Unlock()
		return
	}
	newLayers := make([]uint16, 0, 3)
	for _, l := range layers {
		if l != layer {
			newLayers = append(newLayers, l)
		}
	}
	w.availableLayers.Store(newLayers)
	w.upTrackMu.Unlock()
	// need to immediately switch off unavailable layers
	w.downtrackLayerChange(newLayers, false)
}

func (w *WebRTCReceiver) GetBitrate() [3]uint64 {
	var br [3]uint64
	w.bufferMu.RLock()
	defer w.bufferMu.RUnlock()
	for i, buff := range w.buffers {
		if buff != nil {
			if w.HasSpatialLayer(int32(i)) {
				br[i] = buff.Bitrate()
			} else {
				br[i] = 0
			}
		}
	}
	return br
}

func (w *WebRTCReceiver) GetBitrateTemporal() [3][4]uint64 {
	var br [3][4]uint64
	w.bufferMu.RLock()
	defer w.bufferMu.RUnlock()
	for i, buff := range w.buffers {
		if buff != nil {
			tls := make([]uint64, 4)
			if w.HasSpatialLayer(int32(i)) {
				tls = buff.BitrateTemporal()
			}

			for j := 0; j < len(br[i]); j++ {
				br[i][j] = tls[j]
			}
		}
	}
	return br
}

func (w *WebRTCReceiver) GetBitrateTemporalCumulative() [3][4]uint64 {
	// LK-TODO: For SVC tracks, need to accumulate across spatial layers also
	var br [3][4]uint64
	w.bufferMu.RLock()
	defer w.bufferMu.RUnlock()
	for i, buff := range w.buffers {
		if buff != nil {
			tls := make([]uint64, 4)
			if w.HasSpatialLayer(int32(i)) {
				tls = buff.BitrateTemporalCumulative()
			}

			for j := 0; j < len(br[i]); j++ {
				br[i][j] = tls[j]
			}
		}
	}
	return br
}

func (w *WebRTCReceiver) GetMaxTemporalLayer() [3]int32 {
	var tls [3]int32
	w.bufferMu.RLock()
	defer w.bufferMu.RUnlock()
	for i, buff := range w.buffers {
		if buff != nil {
			tls[i] = buff.MaxTemporalLayer()
		}
	}
	return tls
}

// OnCloseHandler method to be called on remote tracked removed
func (w *WebRTCReceiver) OnCloseHandler(fn func()) {
	w.onCloseHandler = fn
}

// DeleteDownTrack removes a DownTrack from a Receiver
func (w *WebRTCReceiver) DeleteDownTrack(peerID string) {
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
		defer w.rtcpMu.Unlock()
		if time.Now().UnixNano()-w.lastPli.get() < w.pliThrottle {
			return
		}
		w.lastPli.set(time.Now().UnixNano())
	}

	w.rtcpCh <- p
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

func (w *WebRTCReceiver) RetransmitPackets(track *DownTrack, packets []packetMeta) error {
	if w.nackWorker.Stopped() {
		return io.ErrClosedPipe
	}
	// LK-TODO: should move down track specific bits into there
	w.nackWorker.Submit(func() {
		src := packetFactory.Get().(*[]byte)
		for _, meta := range packets {
			pktBuff := *src
			w.bufferMu.RLock()
			buff := w.buffers[meta.layer]
			w.bufferMu.RUnlock()
			if buff == nil {
				break
			}
			i, err := buff.GetPacket(pktBuff, meta.sourceSeqNo)
			if err != nil {
				if err == io.EOF {
					break
				}
				continue
			}
			var pkt rtp.Packet
			if err = pkt.Unmarshal(pktBuff[:i]); err != nil {
				continue
			}
			pkt.Header.SequenceNumber = meta.targetSeqNo
			pkt.Header.Timestamp = meta.timestamp
			pkt.Header.SSRC = track.ssrc
			pkt.Header.PayloadType = track.payloadType

			err = track.MaybeTranslateVP8(&pkt, meta)
			if err != nil {
				Logger.Error(err, "translating VP8 packet err")
				continue
			}

			err = track.WriteRTPHeaderExtensions(&pkt.Header)
			if err != nil {
				Logger.Error(err, "writing rtp header extensions err")
				continue
			}

			if _, err = track.writeStream.WriteRTP(&pkt.Header, pkt.Payload); err != nil {
				Logger.Error(err, "Writing rtx packet err")
			} else {
				track.UpdateStats(uint32(i))
			}
		}
		packetFactory.Put(src)
	})
	return nil
}

func (w *WebRTCReceiver) forwardRTP(layer int32) {
	tracker := w.trackers[layer]

	defer func() {
		w.closeOnce.Do(func() {
			w.closed.set(true)
			w.closeTracks()
		})
		if tracker != nil {
			tracker.Stop()
		}
	}()

	pli := []rtcp.Packet{
		&rtcp.PictureLossIndication{SenderSSRC: rand.Uint32(), MediaSSRC: w.SSRC(int(layer))},
	}

	for {
		w.bufferMu.RLock()
		pkt, err := w.buffers[layer].ReadExtended()
		w.bufferMu.RUnlock()
		if err == io.EOF {
			return
		}

		if tracker != nil {
			tracker.Observe(pkt.Packet.SequenceNumber)
		}

		w.downTrackMu.RLock()
		if w.lbThreshold == 0 || len(w.downTracks)-len(w.free) < w.lbThreshold {
			// serial - not enough down tracks for parallelization to outweigh overhead
			for _, dt := range w.downTracks {
				if dt != nil {
					w.writeRTP(layer, dt, pkt, pli)
				}
			}
		} else {
			// parallel - enables much more efficient multi-core utilization
			start := uint64(0)
			end := uint64(len(w.downTracks))

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
							if dt := w.downTracks[i]; dt != nil {
								w.writeRTP(layer, dt, pkt, pli)
							}
						}
					}
				}()
			}
			wg.Wait()
		}
		w.downTrackMu.RUnlock()
	}
}

func (w *WebRTCReceiver) writeRTP(layer int32, dt *DownTrack, pkt *buffer.ExtPacket, pli []rtcp.Packet) {
	// LK-TODO-START
	// Ideally this code should also be moved into the DownTrack
	// structure to keep things modular. Let the down track code
	// make decision on forwarding or not
	// LK-TODO-END
	if w.isSimulcast {
		targetLayer := dt.TargetSpatialLayer()
		currentLayer := dt.CurrentSpatialLayer()
		if targetLayer == layer && currentLayer != targetLayer {
			if pkt.KeyFrame {
				dt.SwitchSpatialLayerDone(targetLayer)
				currentLayer = targetLayer
			} else {
				dt.lastPli.set(time.Now().UnixNano())
				w.SendRTCP(pli)
			}
		}
		// LK-TODO-START
		// Probably need a control here to stop forwarding current layer
		// if the current layer is higher than target layer, i. e. target layer
		// could have been switched down due to bandwidth constraints and
		// continuing to forward higher layer is only going to exacerbate the issue.
		// Note that the client might have also requested a lower layer. So, it
		// would nice to distinguish between client requested downgrade vs bandwidth
		// constrained downgrade and stop higher layer only in the bandwidth
		// constrained case.
		// LK-TODO-END
		if currentLayer != layer {
			dt.pktsDropped.add(1)
			return
		}
	}

	if err := dt.WriteRTP(pkt, layer); err != nil {
		log.Error().Err(err).Str("id", dt.id).Msg("Error writing to down track")
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
	w.downTracks = make([]*DownTrack, 0)
	w.index = make(map[string]int)
	w.free = make(map[int]struct{})
	w.downTrackMu.Unlock()

	w.nackWorker.StopWait()
	if w.onCloseHandler != nil {
		w.onCloseHandler()
	}
}

func (w *WebRTCReceiver) storeDownTrack(track *DownTrack) {
	w.downTrackMu.Lock()
	defer w.downTrackMu.Unlock()

	for idx := range w.free {
		w.index[track.peerID] = idx
		w.downTracks[idx] = track
		delete(w.free, idx)
		return
	}

	w.index[track.peerID] = len(w.downTracks)
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
