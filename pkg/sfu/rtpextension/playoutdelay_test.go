package rtpextension

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPlayoutDelay(t *testing.T) {
	p1 := PlayOutDelay{Min: 100, Max: 200}
	b, err := p1.Marshal()
	require.NoError(t, err)
	require.Len(t, b, playoutDelayExtensionSize)
	var p2 PlayOutDelay
	err = p2.Unmarshal(b)
	require.NoError(t, err)
	require.Equal(t, p1, p2)

	// overflow
	p3 := PlayOutDelay{Min: 100, Max: (1 << 12) * 10}
	_, err = p3.Marshal()
	require.ErrorIs(t, err, errPlayoutDelayOverflow)

	// too small
	p4 := PlayOutDelay{}
	err = p4.Unmarshal([]byte{0x00, 0x00})
	require.ErrorIs(t, err, errTooSmall)

	// from value
	p5 := PlayoutDelayFromValue(1<<12*10, 1<<12*10+10)
	_, err = p5.Marshal()
	require.NoError(t, err)
	require.Equal(t, uint16((1<<12)-1)*10, p5.Min)
	require.Equal(t, uint16((1<<12)-1)*10, p5.Max)
}
