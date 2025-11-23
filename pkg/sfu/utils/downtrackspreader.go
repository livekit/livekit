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
	"sync"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils"
)

type sender interface {
	SubscriberID() livekit.ParticipantID
}

type DownTrackSpreaderParams struct {
	Threshold int
	Logger    logger.Logger
}

type DownTrackSpreader[T sender] struct {
	params DownTrackSpreaderParams

	downTrackMu      sync.RWMutex
	downTracks       map[livekit.ParticipantID]T
	downTracksShadow []T
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

	downTracks := d.downTracksShadow

	d.downTracks = make(map[livekit.ParticipantID]T)
	d.downTracksShadow = nil

	return downTracks
}

func (d *DownTrackSpreader[T]) Store(sender T) {
	d.downTrackMu.Lock()
	defer d.downTrackMu.Unlock()

	d.downTracks[sender.SubscriberID()] = sender
	d.shadowDownTracks()
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

func (d *DownTrackSpreader[T]) Broadcast(writer func(T)) {
	downTracks := d.GetDownTracks()
	if len(downTracks) == 0 {
		return
	}

	threshold := uint64(d.params.Threshold)
	if threshold == 0 {
		threshold = 1000000
	}

	// 100µs is enough to amortize the overhead and provide sufficient load balancing.
	// WriteRTP takes about 50µs on average, so we write to 2 down tracks per loop.
	step := uint64(2)
	utils.ParallelExec(downTracks, threshold, step, writer)
}

func (d *DownTrackSpreader[T]) DownTrackCount() int {
	d.downTrackMu.RLock()
	defer d.downTrackMu.RUnlock()
	return len(d.downTracksShadow)
}

func (d *DownTrackSpreader[T]) shadowDownTracks() {
	d.downTracksShadow = make([]T, 0, len(d.downTracks))
	for _, dt := range d.downTracks {
		d.downTracksShadow = append(d.downTracksShadow, dt)
	}
}
