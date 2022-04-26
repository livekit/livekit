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

	index    map[livekit.ParticipantID]int
	free     map[int]struct{}
	numProcs int
}

func NewDownTrackSpreader(params DownTrackSpreaderParams) *DownTrackSpreader {
	d := &DownTrackSpreader{
		params:   params,
		index:    make(map[livekit.ParticipantID]int),
		free:     make(map[int]struct{}),
		numProcs: runtime.NumCPU(),
	}

	if runtime.GOMAXPROCS(0) < d.numProcs {
		d.numProcs = runtime.GOMAXPROCS(0)
	}

	return d
}

func (d *DownTrackSpreader) Clear() {
	d.index = make(map[livekit.ParticipantID]int)
	d.free = make(map[int]struct{})
}

func (d *DownTrackSpreader) Store(peerID livekit.ParticipantID, numDownTracks int) (int, bool) {
	for idx := range d.free {
		d.index[peerID] = idx
		delete(d.free, idx)
		return idx, true
	}

	d.index[peerID] = numDownTracks
	return numDownTracks, false
}

func (d *DownTrackSpreader) Free(peerID livekit.ParticipantID) (int, bool) {
	idx, ok := d.index[peerID]
	if !ok {
		return idx, ok
	}

	delete(d.index, peerID)
	d.free[idx] = struct{}{}
	return idx, ok
}

func (d *DownTrackSpreader) GetIndex(peerID livekit.ParticipantID) (int, bool) {
	idx, ok := d.index[peerID]
	return idx, ok
}

func (d *DownTrackSpreader) Broadcast(downTracks []TrackSender, layer int32, pkt *buffer.ExtPacket) {
	if d.params.Threshold == 0 || len(downTracks)-len(d.free) < d.params.Threshold {
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
