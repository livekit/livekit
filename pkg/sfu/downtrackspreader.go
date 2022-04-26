package sfu

import (
	"runtime"
	"sync"

	"go.uber.org/atomic"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
)

type DownTrackSpreaderParams struct {
	Threshold int
	Logger    logger.Logger
}

type DownTrackSpreader struct {
	params DownTrackSpreaderParams

	downTrackMu sync.RWMutex
	downTracks  []TrackSender
	index       map[livekit.ParticipantID]int
	free        map[int]struct{}
	numProcs    int
}

func NewDownTrackSpreader(params DownTrackSpreaderParams) *DownTrackSpreader {
	d := &DownTrackSpreader{
		params:     params,
		downTracks: make([]TrackSender, 0),
		index:      make(map[livekit.ParticipantID]int),
		free:       make(map[int]struct{}),
		numProcs:   runtime.NumCPU(),
	}

	if runtime.GOMAXPROCS(0) < d.numProcs {
		d.numProcs = runtime.GOMAXPROCS(0)
	}

	return d
}

func (d *DownTrackSpreader) GetDownTracks() []TrackSender {
	d.downTrackMu.RLock()
	defer d.downTrackMu.RUnlock()

	return d.downTracks
}

func (d *DownTrackSpreader) ResetAndGetDownTracks() []TrackSender {
	d.downTrackMu.Lock()
	defer d.downTrackMu.Unlock()

	downTracks := d.downTracks

	d.index = make(map[livekit.ParticipantID]int)
	d.free = make(map[int]struct{})
	d.downTracks = make([]TrackSender, 0)

	return downTracks
}

func (d *DownTrackSpreader) Store(ts TrackSender) {
	d.downTrackMu.Lock()
	defer d.downTrackMu.Unlock()

	peerID := ts.PeerID()
	for idx := range d.free {
		d.index[peerID] = idx
		delete(d.free, idx)
		d.downTracks[idx] = ts
		return
	}

	d.index[peerID] = len(d.downTracks)
	d.downTracks = append(d.downTracks, ts)
}

func (d *DownTrackSpreader) Free(peerID livekit.ParticipantID) {
	d.downTrackMu.Lock()
	defer d.downTrackMu.Unlock()

	idx, ok := d.index[peerID]
	if !ok {
		return
	}

	delete(d.index, peerID)
	d.downTracks[idx] = nil
	d.free[idx] = struct{}{}
}

func (d *DownTrackSpreader) HasDownTrack(peerID livekit.ParticipantID) bool {
	d.downTrackMu.RLock()
	defer d.downTrackMu.RUnlock()

	_, ok := d.index[peerID]
	return ok
}

func (d *DownTrackSpreader) Broadcast(layer int32, pkt *buffer.ExtPacket) {
	d.downTrackMu.RLock()
	downTracks := d.downTracks
	free := d.free
	d.downTrackMu.RUnlock()

	if d.params.Threshold == 0 || len(downTracks)-len(free) < d.params.Threshold {
		// serial - not enough down tracks for parallelization to outweigh overhead
		for _, dt := range downTracks {
			if dt != nil {
				d.writeRTP(layer, dt, pkt)
			}
		}
	} else {
		// parallel - enables much more efficient multi-core utilization
		start := atomic.NewUint64(0)
		end := uint64(len(downTracks))

		// 100µs is enough to amortize the overhead and provide sufficient load balancing.
		// WriteRTP takes about 50µs on average, so we write to 2 down tracks per loop.
		step := uint64(2)

		var wg sync.WaitGroup
		wg.Add(d.numProcs)
		for p := 0; p < d.numProcs; p++ {
			go func() {
				defer wg.Done()
				for {
					n := start.Add(step)
					if n >= end+step {
						return
					}

					for i := n - step; i < n && i < end; i++ {
						if dt := downTracks[i]; dt != nil {
							d.writeRTP(layer, dt, pkt)
						}
					}
				}
			}()
		}
		wg.Wait()
	}
}

func (d *DownTrackSpreader) writeRTP(layer int32, dt TrackSender, pkt *buffer.ExtPacket) {
	if err := dt.WriteRTP(pkt, layer); err != nil {
		d.params.Logger.Errorw("failed writing to down track", err)
	}
}
