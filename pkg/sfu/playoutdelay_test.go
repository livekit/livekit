package sfu

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/sfu/rtpextension"
	"github.com/livekit/protocol/logger"
)

func TestPlayoutDelay(t *testing.T) {
	stats := buffer.NewRTPStatsSender(buffer.RTPStatsParams{ClockRate: 900000, Logger: logger.GetLogger()})
	c, err := NewPlayoutDelayController(100, 1000, logger.GetLogger(), stats)
	require.NoError(t, err)

	ext := c.GetDelayExtension(100)
	playoutDelayEqual(t, ext, 100, 1000)

	ext = c.GetDelayExtension(105)
	playoutDelayEqual(t, ext, 100, 1000)

	// seq acked before delay changed
	c.OnSeqAcked(65534)
	ext = c.GetDelayExtension(105)
	playoutDelayEqual(t, ext, 100, 1000)

	c.OnSeqAcked(90)
	ext = c.GetDelayExtension(105)
	playoutDelayEqual(t, ext, 100, 1000)

	// seq acked, no extension sent for new packet
	c.OnSeqAcked(103)
	ext = c.GetDelayExtension(106)
	require.Nil(t, ext)

	// delay on change(can't go below min), no extension sent
	c.SetJitter(0)
	ext = c.GetDelayExtension(107)
	require.Nil(t, ext)

	// delay changed, generate new extension to send
	c.SetJitter(50)
	ext = c.GetDelayExtension(108)
	var delay rtpextension.PlayOutDelay
	require.NoError(t, delay.Unmarshal(ext))
	require.Greater(t, delay.Min, uint16(100))

	// can't go above max
	c.SetJitter(10000)
	ext = c.GetDelayExtension(109)
	playoutDelayEqual(t, ext, 1000, 1000)
}

func playoutDelayEqual(t *testing.T, data []byte, min, max uint16) {
	var delay rtpextension.PlayOutDelay
	require.NoError(t, delay.Unmarshal(data))
	require.Equal(t, min, delay.Min)
	require.Equal(t, max, delay.Max)
}
