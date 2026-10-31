package agent

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/psrpc"
)

func TestWeightedRandomSelection_EmptyClaims(t *testing.T) {
	_, err := weightedRandomSelection(nil)
	require.ErrorIs(t, err, psrpc.ErrNoResponse)

	_, err = weightedRandomSelection([]*psrpc.Claim{})
	require.ErrorIs(t, err, psrpc.ErrNoResponse)
}

func TestWeightedRandomSelection_ZeroAffinity(t *testing.T) {
	claims := []*psrpc.Claim{
		{ServerID: "a", Affinity: 0},
		{ServerID: "b", Affinity: 0},
		{ServerID: "c", Affinity: 0},
	}

	seen := make(map[string]bool)
	for i := 0; i < 1000; i++ {
		id, err := weightedRandomSelection(claims)
		require.NoError(t, err)
		seen[id] = true
	}

	for _, c := range claims {
		require.True(t, seen[c.ServerID], "server %s was never selected", c.ServerID)
	}
}

func TestWeightedRandomSelection_Distribution(t *testing.T) {
	affinities := []float32{0, 0.1, 2, 5, 10, 20, 30, 35}
	claims := make([]*psrpc.Claim, len(affinities))
	for i, a := range affinities {
		claims[i] = &psrpc.Claim{
			ServerID: string(rune('A' + i)),
			Affinity: a,
		}
	}

	var totalWeight float64
	for _, a := range affinities {
		totalWeight += float64(a)
	}

	const iterations = 100_000
	counts := make(map[string]int)
	for i := 0; i < iterations; i++ {
		id, err := weightedRandomSelection(claims)
		require.NoError(t, err)
		counts[id]++
	}

	// Server with 0 affinity should never be selected when others have positive weight.
	require.Zero(t, counts[string(rune('A'))], "server with 0 affinity should never be selected")

	for i, a := range affinities {
		if a == 0 {
			continue
		}
		id := string(rune('A' + i))
		expected := float64(a) / totalWeight
		actual := float64(counts[id]) / iterations

		// Allow 2 percentage points of tolerance.
		require.InDelta(t, expected, actual, 0.02,
			"server %s: expected proportion %.4f, got %.4f (affinity %.1f)",
			id, expected, actual, a,
		)
	}

	_ = math.Abs // ensure import is used
}
