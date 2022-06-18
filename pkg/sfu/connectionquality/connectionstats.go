package connectionquality

import (
	"sync"
	"time"

	"github.com/pion/webrtc/v3"
	"go.uber.org/atomic"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/utils"
)

const (
	UpdateInterval = 2 * time.Second
)

type ConnectionStatsParams struct {
	UpdateInterval   time.Duration
	CodecType        webrtc.RTPCodecType
	CodecName        string
	DtxDisabled      bool // RAJA-TODO - fix this
	MimeType         string
	GetDeltaStats    func() map[uint32]*buffer.StreamStatsWithLayers
	GetQualityParams func() *buffer.ConnectionQualityParams
	// RAJA-REMOVE GetIsReducedQuality func() bool
	GetLayerDimension   func(int32) (uint32, uint32)
	GetMaxExpectedLayer func() *livekit.VideoLayer
	Logger              logger.Logger
}

type ConnectionStats struct {
	params ConnectionStatsParams

	onStatsUpdate func(cs *ConnectionStats, stat *livekit.AnalyticsStat)

	lock       sync.RWMutex
	score      float32
	lastUpdate time.Time

	done     chan struct{}
	isClosed atomic.Bool
}

func NewConnectionStats(params ConnectionStatsParams) *ConnectionStats {
	return &ConnectionStats{
		params: params,
		score:  5.0,
		done:   make(chan struct{}),
	}
}

func (cs *ConnectionStats) Start() {
	go cs.updateStatsWorker()
}

func (cs *ConnectionStats) Close() {
	if cs.isClosed.Swap(true) {
		return
	}

	close(cs.done)
}

func (cs *ConnectionStats) OnStatsUpdate(fn func(cs *ConnectionStats, stat *livekit.AnalyticsStat)) {
	cs.onStatsUpdate = fn
}

func (cs *ConnectionStats) GetScore() float32 {
	cs.lock.RLock()
	defer cs.lock.RUnlock()

	return cs.score
}

/* RAJA-REMOVE
func (cs *ConnectionStats) updateScore(streams []*livekit.AnalyticsStream) float32 {
	cs.lock.Lock()
	defer cs.lock.Unlock()

	// Initial interval will have partial data
	if cs.lastUpdate.IsZero() {
		cs.lastUpdate = time.Now()
		cs.score = 5
		return cs.score
	}

	qp := cs.params.GetQualityParams()
	// RAJA-REMOVE var qualityParam buffer.ConnectionQualityParams
	if qp == nil {
		return cs.score
	} else {
		qp = *s
		if math.IsInf(float64(qp.Jitter), 0) {
			qp.Jitter = 0
		}
		if math.IsInf(float64(qp.LossPercentage), 0) {
			qp.LossPercentage = 0
		}
	}

	interval := time.Since(cs.lastUpdate)
	cs.lastUpdate = time.Now()
	if cs.params.CodecType == webrtc.RTPCodecTypeAudio {
		totalBytes, _, _ := cs.getBytesFramesFromStreams(streams)
		cs.score = AudioConnectionScore(interval, int64(totalBytes), qp, cs.params.DtxDisabled)
	} else {
		// get tracks expected max layer and dimensions
		expectedLayer := cs.params.GetMaxExpectedLayer()
		if expectedLayer == nil || utils.SpatialLayerForQuality(expectedLayer.Quality) == buffer.InvalidLayerSpatial {
			return cs.score
		}

		// get bytes/frames and max later from actual stream stats
		totalBytes, totalFrames, maxLayer := cs.getBytesFramesFromStreams(streams)
		var actualHeight uint32
		var actualWidth uint32
		// if data present, but maxLayer == -1 no layer info available, set actual to expected, else fetch
		if maxLayer == buffer.InvalidLayerSpatial && totalBytes > 0 {
			actualHeight = expectedLayer.Height
			actualWidth = expectedLayer.Width
		} else {
			actualWidth, actualHeight = cs.params.GetLayerDimension(maxLayer)
		}

		cs.score = VideoConnectionScore(
			interval,
			int64(totalBytes),
			int64(totalFrames),
			// RAJA-REMOVE &qualityParam,
			qp,
			cs.params.CodecName,
			int32(expectedLayer.Height),
			int32(expectedLayer.Width),
			int32(actualHeight),
			int32(actualWidth),
		)
	}

	return cs.score
}
*/

func (cs *ConnectionStats) updateScore(streams map[uint32]*buffer.StreamStatsWithLayers) float32 {
	cs.lock.Lock()
	defer cs.lock.Unlock()

	// Initial interval will have partial data
	if cs.lastUpdate.IsZero() {
		cs.lastUpdate = time.Now()
		cs.score = 5
		return cs.score
	}

	interval := time.Since(cs.lastUpdate)
	cs.lastUpdate = time.Now()
	switch cs.params.CodecType {
	case webrtc.RTPCodecTypeAudio:
		//_, maxAvailableSpatialLayerStats := cs.getMaxAvailableLayerStats(streams)
		// RAJA-TODO cs.score = AudioConnectionScore(interval, int64(totalBytes), qp, cs.params.DtxDisabled)

	case webrtc.RTPCodecTypeVideo:
		// get tracks expected max layer and dimensions
		maxExpectedLayer := cs.params.GetMaxExpectedLayer()
		if maxExpectedLayer == nil || utils.SpatialLayerForQuality(maxExpectedLayer.Quality) == buffer.InvalidLayerSpatial {
			return cs.score
		}

		maxAvailableSpatialLayer, maxAvailableSpatialLayerStats := cs.getMaxAvailableLayerStats(streams)

		var actualWidth, actualHeight uint32
		if maxAvailableSpatialLayerStats != nil {
			actualWidth, actualHeight = cs.params.GetLayerDimension(maxAvailableSpatialLayer)
		}

		var bytes, frames int64
		var lossPercentage float32
		var jitter float64
		var rtt uint32
		if maxAvailableSpatialLayerStats != nil {
			bytes = int64(maxAvailableSpatialLayerStats.Bytes)
			frames = int64(maxAvailableSpatialLayerStats.Frames)

			packetsExpected := maxAvailableSpatialLayerStats.Packets + maxAvailableSpatialLayerStats.PacketsPadding
			if packetsExpected > 0 {
				lossPercentage = float32(maxAvailableSpatialLayerStats.PacketsLost) / float32(packetsExpected)
			}

			jitter = maxAvailableSpatialLayerStats.JitterMax
			rtt = maxAvailableSpatialLayerStats.RttMax
		}

		cs.score = VideoConnectionScore(
			interval,
			bytes,
			frames,
			&buffer.ConnectionQualityParams{
				LossPercentage: lossPercentage,
				Jitter:         float32(jitter),
				Rtt:            rtt,
			},
			cs.params.CodecName,
			int32(maxExpectedLayer.Height),
			int32(maxExpectedLayer.Width),
			int32(actualHeight),
			int32(actualWidth),
		)
	}

	return cs.score
}

func (cs *ConnectionStats) getStat() *livekit.AnalyticsStat {
	if cs.params.GetDeltaStats == nil {
		return nil
	}

	streams := cs.params.GetDeltaStats()
	if len(streams) == 0 {
		return nil
	}

	analyticsStreams := make([]*livekit.AnalyticsStream, 0, len(streams))
	for ssrc, stream := range streams {
		as := ToAnalyticsStream(ssrc, stream.RTPStats)

		//
		// add video layer if either
		//   1. Simulcast - even if there is only one layer per stream as it provides layer id
		//   2. A stream has multiple layers
		//
		if cs.params.CodecType == webrtc.RTPCodecTypeVideo && (len(streams) > 1 || len(stream.Layers) > 1) {
			for layer, layerStats := range stream.Layers {
				as.VideoLayers = append(as.VideoLayers, ToAnalyticsVideoLayer(layer, layerStats))
			}
		}

		analyticsStreams = append(analyticsStreams, as)
	}

	// RAJA-REMOVE score := cs.updateScore(analyticsStreams)
	score := cs.updateScore(streams)

	return &livekit.AnalyticsStat{
		Score:   score,
		Streams: analyticsStreams,
		Mime:    cs.params.MimeType,
	}
}

func (cs *ConnectionStats) updateStatsWorker() {
	interval := cs.params.UpdateInterval
	if interval == 0 {
		interval = UpdateInterval
	}

	tk := time.NewTicker(interval)
	defer tk.Stop()

	for {
		select {
		case <-cs.done:
			return

		case <-tk.C:
			stat := cs.getStat()
			if stat == nil {
				continue
			}

			if cs.onStatsUpdate != nil {
				cs.onStatsUpdate(cs, stat)
			}
		}
	}
}

func ToAnalyticsStream(ssrc uint32, deltaStats *buffer.RTPDeltaInfo) *livekit.AnalyticsStream {
	return &livekit.AnalyticsStream{
		Ssrc:              ssrc,
		PrimaryPackets:    deltaStats.Packets,
		PrimaryBytes:      deltaStats.Bytes,
		RetransmitPackets: deltaStats.PacketsDuplicate,
		RetransmitBytes:   deltaStats.BytesDuplicate,
		PaddingPackets:    deltaStats.PacketsPadding,
		PaddingBytes:      deltaStats.BytesPadding,
		PacketsLost:       deltaStats.PacketsLost,
		Frames:            deltaStats.Frames,
		Rtt:               deltaStats.RttMax,
		Jitter:            uint32(deltaStats.JitterMax),
		Nacks:             deltaStats.Nacks,
		Plis:              deltaStats.Plis,
		Firs:              deltaStats.Firs,
	}
}

func ToAnalyticsVideoLayer(layer int32, layerStats *buffer.RTPDeltaInfo) *livekit.AnalyticsVideoLayer {
	return &livekit.AnalyticsVideoLayer{
		Layer:   layer,
		Packets: layerStats.Packets + layerStats.PacketsDuplicate + layerStats.PacketsPadding,
		Bytes:   layerStats.Bytes + layerStats.BytesDuplicate + layerStats.BytesPadding,
		Frames:  layerStats.Frames,
	}
}

/* RAJA-REMOVE
func (cs *ConnectionStats) getBytesFramesFromStreams(streams []*livekit.AnalyticsStream) (totalBytes uint64, totalFrames uint32, maxLayer int32) {
	layerStats := make(map[int32]buffer.LayerStats)
	hasLayers := false
	maxLayer = buffer.InvalidLayerSpatial
	for _, stream := range streams {
		// get frames/bytes/packets from video layers if available. Store per layer in layerStats map
		if len(stream.VideoLayers) > 0 {
			hasLayers = true

			layers := stream.VideoLayers
			// find max quality 0(LOW), 1(MED), 2(HIGH) . sort on layer.Layer desc
			sort.Slice(layers, func(i, j int) bool {
				return layers[i].Layer > layers[j].Layer
			})

			layerStats[layers[0].Layer] = buffer.LayerStats{
				Bytes:  layers[0].GetBytes(),
				Frames: layers[0].GetFrames(),
			}
			if layers[0].Layer > maxLayer {
				maxLayer = layers[0].Layer
			}
		} else {
			totalFrames += stream.GetFrames()
			totalBytes += stream.GetPrimaryBytes()
		}
	}
	if hasLayers {
		if stats, ok := layerStats[maxLayer]; ok {
			return stats.Bytes, stats.Frames, maxLayer
		} else {
			return 0, 0, buffer.InvalidLayerSpatial
		}
	}
	return totalBytes, totalFrames, maxLayer
}
*/

func (cs *ConnectionStats) getMaxAvailableLayerStats(streams map[uint32]*buffer.StreamStatsWithLayers) (int32, *buffer.RTPDeltaInfo) {
	maxAvailableLayer := buffer.InvalidLayerSpatial
	var maxAvailableLayerStats *buffer.RTPDeltaInfo
	for _, stream := range streams {
		for layer, layerStats := range stream.Layers {
			if int32(layer) > maxAvailableLayer {
				maxAvailableLayer = int32(layer)
			}
			maxAvailableLayerStats = layerStats
		}
	}

	return maxAvailableLayer, maxAvailableLayerStats
}
