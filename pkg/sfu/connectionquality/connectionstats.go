package connectionquality

import (
	"sync"
	"time"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/utils"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"
)

type Snapshot struct {
	Initialized      bool
	TotalPacketsLost uint32
	HighestSeqNum    uint32
}

type ConnectionStatsParams struct {
	UpdateInterval      time.Duration
	CodecType           webrtc.RTPCodecType
	GetTotalBytes       func() uint64
	GetIsReducedQuality func() bool
}

type ConnectionStats struct {
	lock sync.RWMutex

	params ConnectionStatsParams

	baseSeqNumInitialized bool
	baseSeqNum            uint32
	highestSeqNum         uint32
	totalPacketsLost      uint32
	totalBytes            uint64

	snapshot Snapshot

	maxDelay  uint32
	maxJitter uint32

	nackCount int32
	pliCount  int32
	firCount  int32

	score float64

	onStatsUpdate func(cs *ConnectionStats, stat *livekit.AnalyticsStat)

	done     chan struct{}
	isClosed utils.AtomicFlag
}

func NewConnectionStats(params ConnectionStatsParams) *ConnectionStats {
	return &ConnectionStats{
		params: params,
		score:  4.0,
		done:   make(chan struct{}),
	}
}

func (cs *ConnectionStats) Start() {
	go cs.updateStats()
}

func (cs *ConnectionStats) Close() {
	if !cs.isClosed.TrySet(true) {
		return
	}

	close(cs.done)
}

func (cs *ConnectionStats) OnStatsUpdate(fn func(cs *ConnectionStats, stat *livekit.AnalyticsStat)) {
	cs.onStatsUpdate = fn
}

func (cs *ConnectionStats) RTCPFeedback(packets []rtcp.Packet, expectedSSRC uint32) {
	if cs.isClosed.Get() {
		return
	}

	cs.lock.Lock()
	defer cs.lock.Unlock()

	for _, p := range packets {
		switch pkt := p.(type) {
		case *rtcp.ReceiverReport:
			for _, r := range pkt.Reports {
				if expectedSSRC != 0 && r.SSRC != expectedSSRC {
					continue
				}

				if r.Delay > cs.maxDelay {
					cs.maxDelay = r.Delay
				}

				if r.Jitter > cs.maxJitter {
					cs.maxJitter = r.Jitter
				}

				if !cs.baseSeqNumInitialized {
					cs.baseSeqNumInitialized = true
					cs.baseSeqNum = r.LastSequenceNumber
					cs.highestSeqNum = r.LastSequenceNumber
					cs.totalPacketsLost = r.TotalLost
				}

				if r.LastSequenceNumber > cs.highestSeqNum {
					cs.highestSeqNum = r.LastSequenceNumber
					cs.totalPacketsLost = r.TotalLost
				}
			}

		case *rtcp.TransportLayerNack:
			nackCount := 0
			for _, pair := range pkt.Nacks {
				nackCount += len(pair.PacketList())
			}
			cs.nackCount += int32(nackCount)

		case *rtcp.PictureLossIndication:
			cs.pliCount += 1

		case *rtcp.FullIntraRequest:
			cs.firCount += 1
		}
	}
}

func (cs *ConnectionStats) GetScore() float64 {
	cs.lock.RLock()
	defer cs.lock.RUnlock()

	return cs.score
}

func (cs *ConnectionStats) updateAndGetPercentageLoss() float64 {
	if cs.params.GetTotalBytes != nil {
		cs.totalBytes = cs.params.GetTotalBytes()
	}

	if !cs.snapshot.Initialized {
		cs.snapshot.Initialized = true
		cs.snapshot.HighestSeqNum = cs.highestSeqNum
		cs.snapshot.TotalPacketsLost = cs.totalPacketsLost
	}

	packetsLostInInterval := cs.totalPacketsLost - cs.snapshot.TotalPacketsLost
	expectedPacketsInInterval := cs.highestSeqNum - cs.snapshot.HighestSeqNum
	percentageLoss := float64(0.0)
	if expectedPacketsInInterval > 0 {
		percentageLoss = (float64(packetsLostInInterval) / float64(expectedPacketsInInterval)) * 100
	}

	cs.snapshot.HighestSeqNum = cs.highestSeqNum
	cs.snapshot.TotalPacketsLost = cs.totalPacketsLost

	return percentageLoss
}

func (cs *ConnectionStats) updateStats() {
	tk := time.NewTicker(cs.params.UpdateInterval)
	for {
		select {
		case <-cs.done:
			return

		case <-tk.C:
			cs.lock.Lock()
			pctLoss := cs.updateAndGetPercentageLoss()
			if cs.params.CodecType == webrtc.RTPCodecTypeAudio {
				cs.score = AudioConnectionScore(pctLoss, cs.maxJitter)
			} else {
				isReducedQuality := false
				if cs.params.GetIsReducedQuality != nil {
					isReducedQuality = cs.params.GetIsReducedQuality()
				}
				cs.score = VideoConnectionScore(pctLoss, isReducedQuality)
			}

			totalPacketsReceived := uint32(0)
			if cs.baseSeqNumInitialized {
				totalPacketsReceived = cs.highestSeqNum - cs.baseSeqNum
			}

			stat := &livekit.AnalyticsStat{
				TotalPackets:    uint64(totalPacketsReceived),
				PacketLost:      uint64(cs.totalPacketsLost),
				TotalBytes:      cs.totalBytes,
				Delay:           uint64(cs.maxDelay),
				Jitter:          float64(cs.maxJitter),
				NackCount:       cs.nackCount,
				PliCount:        cs.pliCount,
				FirCount:        cs.firCount,
				ConnectionScore: float32(cs.score),
			}
			cs.lock.Unlock()

			if cs.onStatsUpdate != nil {
				cs.onStatsUpdate(cs, stat)
			}
		}
	}
}
