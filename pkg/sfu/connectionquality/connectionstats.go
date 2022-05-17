package connectionquality

import (
	"strings"
	"sync"
	"time"

	"github.com/pion/webrtc/v3"
	"go.uber.org/atomic"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
)

const (
	UpdateInterval = 2 * time.Second
)

type ConnectionStatsParams struct {
	UpdateInterval      time.Duration
	CodecType           webrtc.RTPCodecType
	GetDeltaStats       func() map[uint32]*buffer.StreamStatsWithLayers
	GetQualityParams    func() *buffer.ConnectionQualityParams
	GetIsReducedQuality func() bool
	Logger              logger.Logger
	MimeType            string
	Codec               string
	DtxDisabled         bool
	GetLayerDimension   func(int32) (uint32, uint32)
	GetMaxExpectedLayer func() int32
}

type ConnectionStats struct {
	params ConnectionStatsParams

	onStatsUpdate func(cs *ConnectionStats, stat *livekit.AnalyticsStat)

	lock  sync.RWMutex
	score float32

	done     chan struct{}
	isClosed atomic.Bool
}

func NewConnectionStats(params ConnectionStatsParams) *ConnectionStats {
	// try to get the codec from passed codec (mime header)
	codec := ""
	codecParsed := strings.Split(strings.ToLower(params.MimeType), "/")
	if len(codecParsed) > 1 {
		codec = codecParsed[1]
	}
	params.Codec = codec
	return &ConnectionStats{
		params: params,
		score:  4.0,
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

func (cs *ConnectionStats) updateScore(streams []*livekit.AnalyticsStream) float32 {
	cs.lock.Lock()
	defer cs.lock.Unlock()

	s := cs.params.GetQualityParams()
	if s == nil {
		return cs.score
	}

	interval := cs.params.UpdateInterval
	if interval == 0 {
		interval = UpdateInterval
	}
	if cs.params.CodecType == webrtc.RTPCodecTypeAudio {
		totalBytes, _, _ := cs.getBytesFramesFromStreams(streams)
		cs.score = AudioConnectionScore(interval, int64(totalBytes), s, cs.params.DtxDisabled)
	} else {
		// get tracks expected max layer and dimensions
		expectedLayer := cs.params.GetMaxExpectedLayer()
		if expectedLayer == int32(livekit.VideoQuality_OFF) {
			return cs.score
		}
		expectedHeight, expectedWidth := cs.params.GetLayerDimension(expectedLayer)

		// get bytes/frames and max later from actual stream stats
		totalBytes, totalFrames, maxLayer := cs.getBytesFramesFromStreams(streams)
		var actualHeight uint32
		var actualWidth uint32
		// if data present, but maxLayer == -1 no layer info available, set actual to expected, else fetch
		if maxLayer == -1 && totalBytes > 0 {
			actualHeight = expectedHeight
			actualWidth = expectedWidth
		} else {
			actualHeight, actualWidth = cs.params.GetLayerDimension(maxLayer)
		}

		cs.score = VideoConnectionScore(interval, int64(totalBytes), int64(totalFrames), s, cs.params.Codec,
			int32(expectedHeight), int32(expectedWidth), int32(actualHeight), int32(actualWidth))
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
				as.VideoLayers = append(as.VideoLayers, ToAnalyticsVideoLayer(layer, &layerStats))
			}
		}

		analyticsStreams = append(analyticsStreams, as)
	}

	score := cs.updateScore(analyticsStreams)

	return &livekit.AnalyticsStat{
		Score:   score,
		Streams: analyticsStreams,
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

func ToAnalyticsVideoLayer(layer int, layerStats *buffer.LayerStats) *livekit.AnalyticsVideoLayer {
	return &livekit.AnalyticsVideoLayer{
		Layer:   int32(layer),
		Packets: layerStats.Packets,
		Bytes:   layerStats.Bytes,
		Frames:  layerStats.Frames,
	}
}

func (cs *ConnectionStats) getBytesFramesFromStreams(streams []*livekit.AnalyticsStream) (totalBytes uint64, totalFrames uint32, maxLayer int32) {
	type stat struct {
		TotalBytes  uint64
		TotalFrames uint32
	}
	layerStats := make(map[int32]stat)

	maxLayer = -1
	hasLayers := false
	for _, stream := range streams {
		// get frames/bytes/packets from video layers if available. Store per layer in layerStats map
		if len(stream.VideoLayers) > 0 {
			hasLayers = true
			// find max quality 0(LOW), 1(MED), 2(HIGH), 3(OFF)
			videoQuality := int32(-1)
			for _, layer := range stream.VideoLayers {
				if stats, ok := layerStats[layer.Layer]; ok {
					stats.TotalFrames += layer.GetFrames()
					stats.TotalBytes += layer.GetBytes()
					layerStats[layer.Layer] = stats
				} else {
					newStats := stat{
						TotalBytes:  layer.GetBytes(),
						TotalFrames: layer.GetFrames(),
					}
					layerStats[layer.Layer] = newStats
				}

				// if layer is off or of lower quality than processed, skip updating maxLayer
				if (layer.Layer == int32(livekit.VideoQuality_OFF)) || (videoQuality > layer.Layer) {
					continue
				}
				videoQuality = layer.Layer
				if videoQuality > maxLayer {
					maxLayer = videoQuality
				}
			}
		} else {
			totalFrames += stream.Frames
			totalBytes += stream.GetPrimaryBytes() + stream.GetPaddingBytes() + stream.GetRetransmitBytes()
		}
	}

	// if we had layers, then check for layerStats map for maxLayer stats
	if hasLayers {
		if maxLayer == -1 {
			maxLayer = int32(livekit.VideoQuality_OFF)
			return 0, 0, maxLayer
		} else {
			stats := layerStats[maxLayer]
			return stats.TotalBytes, stats.TotalFrames, maxLayer
		}
	} else {
		return totalBytes, totalFrames, -1
	}
}
