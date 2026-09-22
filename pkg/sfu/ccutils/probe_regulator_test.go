// Copyright 2026 LiveKit, Inc.
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

package ccutils

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestProbeRegulatorIntervalBackoff verifies that repeated congestion signals
// grow probeInterval by BackoffFactor without losing sub-second precision.
//
// The interval formula must use millisecond resolution (like probeDuration does)
// rather than second resolution, otherwise each multiplication step truncates
// the fractional part and the interval grows more slowly than intended.
//
// Example with BackoffFactor=1.5 starting from 3 s:
//
//	correct:   3 s → 4.5 s → 6.75 s → 10.125 s
//	truncated: 3 s → 4 s   → 6 s   →  9 s
func TestProbeRegulatorIntervalBackoff(t *testing.T) {
	cfg := DefaultProbeRegulatorConfig // BackoffFactor=1.5, BaseInterval=3s

	r := NewProbeRegulator(ProbeRegulatorParams{Config: cfg})
	require.Equal(t, cfg.BaseInterval, r.probeInterval, "initial probeInterval must equal BaseInterval")

	// Compute the exact expected value using millisecond arithmetic (the correct path).
	expected := time.Duration(float64(cfg.BaseInterval.Milliseconds())*cfg.BackoffFactor) * time.Millisecond

	r.ProbeSignal(ProbeSignalCongesting, time.Time{})

	require.Equal(t, expected, r.probeInterval,
		"after one congestion signal with BackoffFactor=%v from %v, probeInterval must be %v, not %v",
		cfg.BackoffFactor, cfg.BaseInterval, expected, r.probeInterval,
	)
}
