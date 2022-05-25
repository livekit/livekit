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

	downTrackMu      sync.RWMutex
	downTracks       map[livekit.ParticipantID]TrackSender
	downTracksShadow []TrackSender
	numProcs         int
}

func NewDownTrackSpreader(params DownTrackSpreaderParams) *DownTrackSpreader {
	d := &DownTrackSpreader{
		params:     params,
		downTracks: make(map[livekit.ParticipantID]TrackSender),
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

	return d.downTracksShadow
}

func (d *DownTrackSpreader) ResetAndGetDownTracks() []TrackSender {
	d.downTrackMu.Lock()
	defer d.downTrackMu.Unlock()

	downTracks := d.downTracksShadow

	d.downTracks = make(map[livekit.ParticipantID]TrackSender, 0)
	d.downTracksShadow = nil

	return downTracks
}

func (d *DownTrackSpreader) Store(ts TrackSender) {
	d.downTrackMu.Lock()
	defer d.downTrackMu.Unlock()

	d.downTracks[ts.PeerID()] = ts
	d.shadowDownTracks()
}

func (d *DownTrackSpreader) Free(peerID livekit.ParticipantID) {
	d.downTrackMu.Lock()
	defer d.downTrackMu.Unlock()

	delete(d.downTracks, peerID)
	d.shadowDownTracks()
}

func (d *DownTrackSpreader) HasDownTrack(peerID livekit.ParticipantID) bool {
	d.downTrackMu.RLock()
	defer d.downTrackMu.RUnlock()

	_, ok := d.downTracks[peerID]
	return ok
}

func (d *DownTrackSpreader) Broadcast(layer int32, pkt *buffer.ExtPacket) {
	d.downTrackMu.RLock()
	downTracks := d.downTracksShadow
	d.downTrackMu.RUnlock()

	if d.params.Threshold == 0 || (len(downTracks)) < d.params.Threshold {
		// serial - not enough down tracks for parallelization to outweigh overhead
		for _, dt := range downTracks {
			d.writeRTP(layer, dt, pkt)
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
						d.writeRTP(layer, downTracks[i], pkt)
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

func (d *DownTrackSpreader) shadowDownTracks() {
	d.downTracksShadow = make([]TrackSender, 0, len(d.downTracks))
	for _, dt := range d.downTracks {
		d.downTracksShadow = append(d.downTracksShadow, dt)
	}
}
