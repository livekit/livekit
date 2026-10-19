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

package utils

import (
	"runtime"
	"sync"
	"sync/atomic"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils"
)

// 100µs is enough to amortize the overhead and provide sufficient load balancing.
// WriteRTP takes about 50µs on average, so we write to 2 down tracks per loop.
const broadcastStep = 2

type DownTrackSpreaderParams struct {
	Threshold int
	Logger    logger.Logger
}

type DownTrackSpreader[T sender] struct {
	params DownTrackSpreaderParams

	downTrackMu      sync.RWMutex
	downTracks       map[livekit.ParticipantID]T
	downTracksShadow []T
	closed           bool
}

func NewDownTrackSpreader[T sender](params DownTrackSpreaderParams) *DownTrackSpreader[T] {
	d := &DownTrackSpreader[T]{
		params:     params,
		downTracks: make(map[livekit.ParticipantID]T),
	}

	return d
}

func (d *DownTrackSpreader[T]) GetDownTracks() []T {
	d.downTrackMu.RLock()
	defer d.downTrackMu.RUnlock()
	return d.downTracksShadow
}

func (d *DownTrackSpreader[T]) ResetAndGetDownTracks() []T {
	d.downTrackMu.Lock()
	defer d.downTrackMu.Unlock()

	return d.resetAndGetDownTracksLocked()
}

// CloseAndGetDownTracks terminally closes the spreader and drains all senders.
// Once it returns, TryStore will reject every subsequent sender.
func (d *DownTrackSpreader[T]) CloseAndGetDownTracks() []T {
	d.downTrackMu.Lock()
	defer d.downTrackMu.Unlock()

	d.closed = true
	return d.resetAndGetDownTracksLocked()
}

func (d *DownTrackSpreader[T]) Store(sender T) {
	_ = d.TryStore(sender)
}

// TryStore stores sender unless the spreader has been terminally closed.
func (d *DownTrackSpreader[T]) TryStore(sender T) bool {
	d.downTrackMu.Lock()
	defer d.downTrackMu.Unlock()

	if d.closed {
		return false
	}

	d.downTracks[sender.SubscriberID()] = sender
	d.shadowDownTracks()
	return true
}

func (d *DownTrackSpreader[T]) Free(subscriberID livekit.ParticipantID) {
	d.downTrackMu.Lock()
	defer d.downTrackMu.Unlock()

	delete(d.downTracks, subscriberID)
	d.shadowDownTracks()
}

func (d *DownTrackSpreader[T]) HasDownTrack(subscriberID livekit.ParticipantID) bool {
	d.downTrackMu.RLock()
	defer d.downTrackMu.RUnlock()

	_, ok := d.downTracks[subscriberID]
	return ok
}

// snapshot returns the current down tracks and the parallelization threshold
func (d *DownTrackSpreader[T]) snapshot() ([]T, int) {
	d.downTrackMu.RLock()
	downTracks := d.downTracksShadow
	threshold := d.params.Threshold
	d.downTrackMu.RUnlock()

	if threshold == 0 {
		threshold = 1000000
	}
	return downTracks, threshold
}

func (d *DownTrackSpreader[T]) Broadcast(writer func(T)) {
	downTracks, threshold := d.snapshot()
	if len(downTracks) == 0 {
		return
	}

	utils.ParallelExec(downTracks, uint64(threshold), broadcastStep, writer)
}

func (d *DownTrackSpreader[T]) DownTrackCount() int {
	d.downTrackMu.RLock()
	defer d.downTrackMu.RUnlock()
	return len(d.downTracksShadow)
}

func (d *DownTrackSpreader[T]) resetAndGetDownTracksLocked() []T {
	downTracks := d.downTracksShadow
	d.downTracks = make(map[livekit.ParticipantID]T)
	d.downTracksShadow = nil
	return downTracks
}

func (d *DownTrackSpreader[T]) shadowDownTracks() {
	d.downTracksShadow = make([]T, 0, len(d.downTracks))
	for _, dt := range d.downTracks {
		d.downTracksShadow = append(d.downTracksShadow, dt)
	}
}

func (d *DownTrackSpreader[T]) SetThreshold(threshold int) {
	d.downTrackMu.Lock()
	d.params.Threshold = threshold
	d.downTrackMu.Unlock()
}

// ------------------------------------------------

type sender interface {
	SubscriberID() livekit.ParticipantID
}

type rtpWriter[P any] interface {
	WriteRTP(pkt P, layer int32) int32
}

// rtpBroadcast is the shared state of one parallel BroadcastRTP, it carries the
// packet and layer so that no closure has to be allocated per packet
type rtpBroadcast[T rtpWriter[P], P any] struct {
	downTracks []T
	pkt        P
	layer      int32
	next       atomic.Uint64
	written    atomic.Int32
	wg         sync.WaitGroup
}

func (b *rtpBroadcast[T, P]) run() {
	defer b.wg.Done()

	var written int32
	end := uint64(len(b.downTracks))
	for {
		n := b.next.Add(broadcastStep)
		if n >= end+broadcastStep {
			break
		}
		for i := n - broadcastStep; i < n && i < end; i++ {
			written += b.downTracks[i].WriteRTP(b.pkt, b.layer)
		}
	}
	b.written.Add(written)
}

// BroadcastRTP writes pkt to every down track and returns how many accepted it.
// Below the threshold it runs on the caller without allocating; above it, the
// only allocations are the shared state and the worker funcval.
func BroadcastRTP[T interface {
	sender
	rtpWriter[P]
}, P any](d *DownTrackSpreader[T], pkt P, layer int32) int32 {
	downTracks, threshold := d.snapshot()
	if len(downTracks) < threshold {
		var written int32
		for _, dt := range downTracks {
			written += dt.WriteRTP(pkt, layer)
		}
		return written
	}

	numWorkers := min(runtime.NumCPU(), len(downTracks))
	b := &rtpBroadcast[T, P]{downTracks: downTracks, pkt: pkt, layer: layer}
	b.wg.Add(numWorkers)
	worker := b.run
	for i := 0; i < numWorkers; i++ {
		go worker()
	}
	b.wg.Wait()

	return b.written.Load()
}
