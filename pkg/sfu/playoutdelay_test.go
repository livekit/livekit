// Copyright 2024 LiveKit, Inc.
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

package sfu

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	pd "github.com/livekit/livekit-server/pkg/sfu/rtpextension/playoutdelay"
	"github.com/livekit/livekit-server/pkg/sfu/rtpstats"
	"github.com/livekit/protocol/logger"
)

func TestPlayoutDelay(t *testing.T) {
	stats := rtpstats.NewRTPStatsSender(rtpstats.RTPStatsParams{ClockRate: 900000, Logger: logger.GetLogger()}, 128)
	c, err := NewPlayoutDelayController(100, 120, logger.GetLogger(), stats)
	require.NoError(t, err)

	ext := c.GetDelayExtension(100)
	playoutDelayEqual(t, ext, 100, 120)

	ext = c.GetDelayExtension(105)
	playoutDelayEqual(t, ext, 100, 120)

	// seq acked before delay changed
	c.OnSeqAcked(65534)
	ext = c.GetDelayExtension(105)
	playoutDelayEqual(t, ext, 100, 120)

	c.OnSeqAcked(90)
	ext = c.GetDelayExtension(105)
	playoutDelayEqual(t, ext, 100, 120)

	// seq acked, no extension sent for new packet
	c.OnSeqAcked(103)
	ext = c.GetDelayExtension(106)
	require.Nil(t, ext)

	// delay on change(can't go below min), no extension sent
	c.SetJitter(0)
	ext = c.GetDelayExtension(107)
	require.Nil(t, ext)

	// delay changed, generate new extension to send
	time.Sleep(200 * time.Millisecond)
	c.SetJitter(50)
	t.Log(c.currentDelay, c.state.Load())
	ext = c.GetDelayExtension(108)
	var delay pd.PlayOutDelay
	require.NoError(t, delay.Unmarshal(ext))
	require.Greater(t, delay.Min, uint16(100))

	// can't go above max
	time.Sleep(200 * time.Millisecond)
	c.SetJitter(10000)
	ext = c.GetDelayExtension(109)
	playoutDelayEqual(t, ext, 120, 120)
}

func playoutDelayEqual(t *testing.T, data []byte, min, max uint16) {
	var delay pd.PlayOutDelay
	require.NoError(t, delay.Unmarshal(data))
	require.Equal(t, min, delay.Min)
	require.Equal(t, max, delay.Max)
}
