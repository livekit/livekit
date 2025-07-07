// Copyright 2023 LiveKit, Inc.
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

package selector_test

import (
	"testing"

	"github.com/livekit/protocol/livekit"

	"github.com/livekit/livekit-server/pkg/routing/selector"
)

func SortByTest(t *testing.T, sortBy string) {
	sel := selector.SystemLoadSelector{SortBy: sortBy, Algorithm: "lowest"}
	nodes := []*livekit.Node{nodeLoadLow, nodeLoadMedium, nodeLoadHigh}

	for i := 0; i < 5; i++ {
		node, err := sel.SelectNode(nodes)
		if err != nil {
			t.Error(err)
		}
		if node != nodeLoadLow {
			t.Error("selected the wrong node for SortBy:", sortBy)
		}
	}
}

func TestSortByErrors(t *testing.T) {
	sel := selector.SystemLoadSelector{Algorithm: "lowest"}
	nodes := []*livekit.Node{nodeLoadLow, nodeLoadMedium, nodeLoadHigh}

	// Test unset sort by option error
	_, err := sel.SelectNode(nodes)
	if err != selector.ErrSortByNotSet {
		t.Error("shouldn't allow empty sortBy")
	}

	// Test unknown sort by option error
	sel.SortBy = "testFail"
	_, err = sel.SelectNode(nodes)
	if err != selector.ErrSortByUnknown {
		t.Error("shouldn't allow unknown sortBy")
	}
}

func TestSortBy(t *testing.T) {
	sortByTests := []string{"sysload", "cpuload", "rooms", "clients", "tracks", "bytespersec"}

	for _, sortBy := range sortByTests {
		SortByTest(t, sortBy)
	}
}
