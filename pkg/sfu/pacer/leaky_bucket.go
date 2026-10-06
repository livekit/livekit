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

package pacer

import (
	"sync"
	"time"

	"github.com/frostbyte73/core"
	"github.com/gammazero/deque"
	"github.com/livekit/livekit-server/pkg/sfu/bwe"
	"github.com/livekit/livekit-server/pkg/sfu/ccutils"
	"github.com/livekit/protocol/logger"
)

const (
	maxOvershootFactor = 2.0

	// drop the stalest frames once the queue holds more than this much
	// media at the active pacing rate, so sustained input above the rate
	// does not grow latency without bound
	maxQueuedSeconds = 0.5
)

type LeakyBucket struct {
	*Base

	logger logger.Logger

	lock         sync.RWMutex
	packets      deque.Deque[*Packet]
	queuedBytes  int
	interval     time.Duration
	bitrate      int
	probeBitrate int
	stop         core.Fuse
}

func NewLeakyBucket(logger logger.Logger, bwe bwe.BWE, interval time.Duration, bitrate int) *LeakyBucket {
	l := &LeakyBucket{
		Base:     NewBase(logger, bwe),
		logger:   logger,
		interval: interval,
		bitrate:  bitrate,
	}
	l.packets.SetBaseCap(512)

	go l.sendWorker()
	return l
}

func (l *LeakyBucket) SetInterval(interval time.Duration) {
	l.lock.Lock()
	defer l.lock.Unlock()

	l.interval = interval
}

func (l *LeakyBucket) SetBitrate(bitrate int) {
	l.lock.Lock()
	defer l.lock.Unlock()

	l.bitrate = bitrate
	l.dropExcessLocked()
}

// a probe cluster measures a rate above the current estimate, pace to the
// cluster target while it is active. pacing probes at the rate under test
// would hold the probe back so its result could not exceed that rate
func (l *LeakyBucket) StartProbeCluster(pci ccutils.ProbeClusterInfo) {
	l.Base.StartProbeCluster(pci)

	l.lock.Lock()
	defer l.lock.Unlock()

	l.probeBitrate = pci.Goal.DesiredBps
}

func (l *LeakyBucket) EndProbeCluster(probeClusterId ccutils.ProbeClusterId) ccutils.ProbeClusterInfo {
	pci := l.Base.EndProbeCluster(probeClusterId)
	if pci.Id != ccutils.ProbeClusterIdInvalid {
		l.lock.Lock()
		l.probeBitrate = 0
		l.dropExcessLocked()
		l.lock.Unlock()
	}

	return pci
}

func (l *LeakyBucket) Stop() {
	l.stop.Break()
}

func (l *LeakyBucket) Enqueue(p *Packet) {
	l.lock.Lock()
	defer l.lock.Unlock()

	l.packets.PushBack(p)
	l.queuedBytes += p.size()

	l.dropExcessLocked()
}

// dropExcessLocked discards whole frames from the front of the queue when it
// exceeds its bound, so the media left in the queue stays decodable. the
// frame being assembled (the packets after the last frame end in the queue)
// is kept, so a burst still arriving is not truncated by its own arrival,
// and the newest packet is never dropped, so an oversized packet is still
// delivered. dropping media is left to the receiver's normal loss handling
// to recover from; that beats delivering media that is arbitrarily late
func (l *LeakyBucket) dropExcessLocked() {
	limit := l.maxQueueBytesLocked()
	if l.queuedBytes <= limit {
		return
	}

	// packets after the last frame end (RTP marker) are the frame being
	// assembled. with no frame end in the queue, protect only the newest
	// packet
	protectFrom := l.packets.Len() - 1
	for i := l.packets.Len() - 1; i >= 0; i-- {
		if h := l.packets.At(i).Header; h != nil && h.Marker {
			protectFrom = i + 1
			break
		}
	}

	for l.queuedBytes > limit && l.packets.Len() > protectFrom {
		// drop the oldest frame: up to and including the first frame end
		// ahead, or a single packet when no frame end is in reach
		drop := 0
		for i := 0; i < protectFrom; i++ {
			if h := l.packets.At(i).Header; h != nil && h.Marker {
				drop = i + 1
				break
			}
		}
		if drop == 0 {
			if protectFrom == 0 {
				break
			}
			drop = 1
		}

		for i := 0; i < drop; i++ {
			p := l.packets.PopFront()
			l.queuedBytes -= p.size()
			releasePacket(p)
		}
		protectFrom -= drop
	}
}

func (l *LeakyBucket) maxQueueBytesLocked() int {
	bitrate := l.bitrate
	if l.probeBitrate > bitrate {
		bitrate = l.probeBitrate
	}

	return int(float64(bitrate) / 8.0 * maxQueuedSeconds)
}

func (l *LeakyBucket) sendWorker() {
	l.lock.RLock()
	interval := l.interval
	l.lock.RUnlock()
	var bitrate int

	timer := time.NewTimer(interval)
	overage := 0

	for {
		<-timer.C

		l.lock.RLock()
		interval = l.interval
		bitrate = l.bitrate
		probeBitrate := l.probeBitrate
		l.lock.RUnlock()

		// pace at the probe target while a cluster is active, it is expected
		// to be above the bandwidth estimate
		if probeBitrate > bitrate {
			bitrate = probeBitrate
		}

		// calculate number of bytes that can be sent in this interval
		// adjusting for overage.
		intervalBytes := int(interval.Seconds() * float64(bitrate) / 8.0)
		maxOvershootBytes := int(float64(intervalBytes) * maxOvershootFactor)
		toSendBytes := intervalBytes - overage
		if toSendBytes < 0 {
			// too much overage, wait for next interval
			overage = -toSendBytes
			timer.Reset(interval)
			continue
		}

		// do not allow too much overshoot in an interval
		if toSendBytes > maxOvershootBytes {
			toSendBytes = maxOvershootBytes
		}

		for {
			if l.stop.IsBroken() {
				return
			}

			l.lock.Lock()
			if l.packets.Len() == 0 {
				l.lock.Unlock()
				// allow overshoot in next interval with shortage in this interval
				overage = -toSendBytes
				timer.Reset(interval)
				break
			}
			p := l.packets.PopFront()
			l.queuedBytes -= p.size()
			l.lock.Unlock()

			written, _ := l.Base.SendPacket(p)
			toSendBytes -= written
			if toSendBytes < 0 {
				// overage, wait for next interval
				overage = -toSendBytes
				timer.Reset(interval)
				break
			}
		}
	}
}

// ------------------------------------------------
