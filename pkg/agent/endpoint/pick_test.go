// Copyright 2026 LiveKit, Inc.

package endpoint

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestP2CChoice(t *testing.T) {
	// a single candidate is always chosen
	require.Equal(t, 0, p2c(1, func(int) int { return 42 }))

	// with two candidates both are always drawn, so the lower-load one wins
	// deterministically regardless of the random draw
	for i := 0; i < 100; i++ {
		require.Equal(t, 1, p2c(2, func(i int) int { return []int{5, 2}[i] }))
		require.Equal(t, 0, p2c(2, func(i int) int { return []int{2, 5}[i] }))
	}

	// larger n: the pick is always in range
	for i := 0; i < 500; i++ {
		idx := p2c(5, func(int) int { return 0 })
		require.GreaterOrEqual(t, idx, 0)
		require.Less(t, idx, 5)
	}

	// one clearly-idle candidate among four busy ones: whenever it is one of the
	// two draws it wins, so it is picked far more than its 1/n share and the busy
	// ones are relieved (this is the whole point of power-of-two-choices)
	loads := []int{0, 100, 100, 100, 100}
	low := 0
	for i := 0; i < 4000; i++ {
		if p2c(len(loads), func(i int) int { return loads[i] }) == 0 {
			low++
		}
	}
	// P(idle drawn) = 1 - (4/5)(3/4) = 0.4, and it always wins when drawn
	require.Greater(t, low, 1000)
	require.Less(t, low, 2400)
}
