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

package rtc

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSubscribePacer(t *testing.T) {
	t.Run("off without rates", func(t *testing.T) {
		require.Nil(t, NewSubscribePacer(nil))
		require.Nil(t, NewSubscribePacer([]float64{0, -5}))

		var p *SubscribePacer
		require.Nil(t, p.Delays(time.Now()))
	})

	t.Run("one second of burst, then one token per interval", func(t *testing.T) {
		p := NewSubscribePacer([]float64{10, 100})
		now := time.Now()

		for i := 0; i < 10; i++ {
			require.Equal(t, map[string]time.Duration{"10": 0, "100": 0}, p.Delays(now), "subscribe %d", i)
		}
		require.Equal(t, map[string]time.Duration{"10": 100 * time.Millisecond, "100": 0}, p.Delays(now))
		require.Equal(t, map[string]time.Duration{"10": 200 * time.Millisecond, "100": 0}, p.Delays(now))
	})

	t.Run("a burst that outruns the rate waits in line", func(t *testing.T) {
		p := NewSubscribePacer([]float64{50})
		now := time.Now()

		var last time.Duration
		for i := 0; i < 150; i++ {
			last = p.Delays(now)["50"]
		}
		// 50 go at once, the other 100 drain at 50 per second
		require.Equal(t, 2*time.Second, last)
	})

	t.Run("a rate below one per second does not delay the first subscribe", func(t *testing.T) {
		p := NewSubscribePacer([]float64{0.5})
		now := time.Now()

		require.Equal(t, time.Duration(0), p.Delays(now)["0.5"])
		require.Equal(t, 2*time.Second, p.Delays(now)["0.5"])
	})

	t.Run("a sample taken earlier than the last one waits from its own time", func(t *testing.T) {
		p := NewSubscribePacer([]float64{10})
		now := time.Now()

		for i := 0; i < 10; i++ {
			p.Delays(now)
		}
		// the next token is free at now + 1 s, and the burst covers 900 ms of it
		require.Equal(t, 600*time.Millisecond, p.Delays(now.Add(-500 * time.Millisecond))["10"])
	})

	t.Run("duplicate rates make one bucket", func(t *testing.T) {
		p := NewSubscribePacer([]float64{50, 10, 50})
		require.Len(t, p.buckets, 2)
	})

	t.Run("refills while idle", func(t *testing.T) {
		p := NewSubscribePacer([]float64{10})
		now := time.Now()

		for i := 0; i < 12; i++ {
			p.Delays(now)
		}
		require.Equal(t, time.Duration(0), p.Delays(now.Add(2 * time.Second))["10"])
	})
}
