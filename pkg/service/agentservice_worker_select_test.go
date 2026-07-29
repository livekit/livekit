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

package service

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/livekit-server/pkg/agent"
)

func TestPickWorkerWeightedByLoadNeverReturnsIneligible(t *testing.T) {
	unavailable := &agent.Worker{}
	eligibleA := &agent.Worker{}
	eligibleB := &agent.Worker{}

	// Use loads that are not dyadic fractions so float32 residuals are likely when r == 1.
	normalizedLoads := map[*agent.Worker]float32{
		eligibleA: 0.1,
		eligibleB: 0.2,
	}
	availableSum := float32(0.3)

	for _, r := range []float32{0, 0.5, 0.999999, 1.0} {
		for i := 0; i < 200; i++ {
			picked := pickWorkerWeightedByLoad(normalizedLoads, availableSum, r)
			require.NotNil(t, picked)
			require.NotEqual(t, unavailable, picked, "fallback must not return an ineligible worker")
			require.Contains(t, []*agent.Worker{eligibleA, eligibleB}, picked)
		}
	}
}

func TestPickWorkerWeightedByLoadEmpty(t *testing.T) {
	require.Nil(t, pickWorkerWeightedByLoad(nil, 1, 0.5))
	require.Nil(t, pickWorkerWeightedByLoad(map[*agent.Worker]float32{}, 1, 0.5))
}
