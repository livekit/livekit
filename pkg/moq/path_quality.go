// Copyright 2026 LiveKit, Inc.
// SPDX-License-Identifier: Apache-2.0

package moq

import (
	"sync"
	"time"

	"github.com/livekit/protocol/livekit"
	"github.com/quic-go/quic-go"

	"github.com/livekit/livekit-server/pkg/sfu/connectionquality"
)

// Sample once per room quality interval. Join-time and per-subscriber reads
// must not consume the packet deltas used by the next periodic report.
type pathQuality struct {
	mu          sync.Mutex
	at          time.Time
	sent, lost  uint64
	lossPercent float64
}

func (p *moqPeer) PathQuality() (float32, livekit.ConnectionQuality) {
	q := &p.path
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.update(time.Now(), p.conn.ConnectionStats())
}

func (q *pathQuality) update(now time.Time, stats quic.ConnectionStats) (float32, livekit.ConnectionQuality) {
	if q.at.IsZero() || now.Sub(q.at) >= connectionquality.UpdateInterval {
		if !q.at.IsZero() && stats.PacketsSent > q.sent {
			var lost uint64
			// quic-go can reverse a loss declaration when a late ACK arrives.
			if stats.PacketsLost > q.lost {
				lost = stats.PacketsLost - q.lost
			}
			sent := stats.PacketsSent - q.sent
			q.lossPercent = 100 * float64(min(sent, lost)) / float64(sent)
		}
		q.at, q.sent, q.lost = now, stats.PacketsSent, stats.PacketsLost
	}
	return connectionquality.PathScore(stats.SmoothedRTT, q.lossPercent)
}
