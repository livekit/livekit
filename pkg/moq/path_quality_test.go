// Copyright 2026 LiveKit, Inc.
// SPDX-License-Identifier: Apache-2.0

package moq

import (
	"testing"
	"time"

	"github.com/livekit/protocol/livekit"
	"github.com/quic-go/quic-go"
	"github.com/stretchr/testify/require"

	"github.com/livekit/livekit-server/pkg/sfu/connectionquality"
)

func TestPathQuality(t *testing.T) {
	var q pathQuality
	at := time.Now()
	stats := quic.ConnectionStats{PacketsSent: 100, SmoothedRTT: 25 * time.Millisecond}
	_, initial := q.update(at, stats)
	require.Equal(t, livekit.ConnectionQuality_EXCELLENT, initial)
	stats.PacketsSent, stats.PacketsLost = 200, 8
	_, cached := q.update(at.Add(time.Millisecond), stats)
	require.Equal(t, initial, cached)
	_, degraded := q.update(at.Add(connectionquality.UpdateInterval), stats)
	require.Equal(t, livekit.ConnectionQuality_POOR, degraded)
	// A late acknowledgement undoes a loss declaration.
	stats.PacketsSent, stats.PacketsLost = 300, 7
	_, recovered := q.update(at.Add(2*connectionquality.UpdateInterval), stats)
	require.Equal(t, initial, recovered)
	stats.SmoothedRTT = 2 * time.Second
	_, slow := q.update(at.Add(3*connectionquality.UpdateInterval), stats)
	require.NotEqual(t, initial, slow)
}
