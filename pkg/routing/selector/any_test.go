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

package selector

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/livekit"
)

func createTestNode(id string, cpuLoad float32, numRooms int32, numClients int32, state livekit.NodeState) *livekit.Node {
	return &livekit.Node{
		Id:    id,
		State: state,
		Stats: &livekit.NodeStats{
			UpdatedAt:       time.Now().Unix() - 1, // Recent update to be considered available
			CpuLoad:         cpuLoad,
			NumRooms:        numRooms,
			NumClients:      numClients,
			NumCpus:         4,
			LoadAvgLast1Min: cpuLoad * 4, // Simulate system load
		},
	}
}

func TestAnySelector_SelectNode_TwoChoice(t *testing.T) {
	tests := []struct {
		name        string
		sortBy      string
		algorithm   string
		nodes       []*livekit.Node
		wantErr     string
		expected    string
		notExpected string
	}{
		{
			name:      "successful selection with cpuload sorting",
			sortBy:    "cpuload",
			algorithm: "twochoice",
			nodes: []*livekit.Node{
				createTestNode("node1", 0.8, 5, 10, livekit.NodeState_SERVING),
				createTestNode("node2", 0.3, 2, 5, livekit.NodeState_SERVING),
				createTestNode("node3", 0.6, 3, 8, livekit.NodeState_SERVING),
				createTestNode("node4", 0.9, 6, 12, livekit.NodeState_SERVING),
			},
			wantErr:     "",
			expected:    "",      // Not determinstic selection, so no specific expected node
			notExpected: "node4", // Node with highest load should not be selected
		},
		{
			name:      "successful selection with rooms sorting",
			sortBy:    "rooms",
			algorithm: "twochoice",
			nodes: []*livekit.Node{
				createTestNode("node1", 0.5, 8, 15, livekit.NodeState_SERVING),
				createTestNode("node2", 0.4, 2, 5, livekit.NodeState_SERVING),
				createTestNode("node3", 0.6, 12, 20, livekit.NodeState_SERVING),
			},
			wantErr:     "",
			expected:    "",      // Not determinstic selection, so no specific expected node
			notExpected: "node3", // Node with highest room count should not be selected
		},
		{
			name:      "successful selection with clients sorting",
			sortBy:    "clients",
			algorithm: "twochoice",
			nodes: []*livekit.Node{
				createTestNode("node1", 0.5, 3, 25, livekit.NodeState_SERVING),
				createTestNode("node2", 0.4, 2, 5, livekit.NodeState_SERVING),
				createTestNode("node3", 0.6, 4, 30, livekit.NodeState_SERVING),
			},
			wantErr:     "",
			expected:    "",      // Not determinstic selection, so no specific expected node
			notExpected: "node3", // Node with highest clients should not be selected
		},
		{
			name:      "empty nodes list",
			sortBy:    "cpuload",
			algorithm: "twochoice",
			nodes:     []*livekit.Node{},
			wantErr:   "could not find any available nodes",
		},
		{
			name:      "no available nodes - all unavailable",
			sortBy:    "cpuload",
			algorithm: "twochoice",
			nodes: []*livekit.Node{
				{
					Id:    "node1",
					State: livekit.NodeState_SERVING,
					Stats: &livekit.NodeStats{
						UpdatedAt: time.Now().Unix() - 10, // Too old
						CpuLoad:   0.3,
					},
				},
			},
			wantErr: "could not find any available nodes",
		},
		{
			name:      "no available nodes - not serving",
			sortBy:    "cpuload",
			algorithm: "twochoice",
			nodes: []*livekit.Node{
				{
					Id:    "node1",
					State: livekit.NodeState_SHUTTING_DOWN,
					Stats: &livekit.NodeStats{
						UpdatedAt: time.Now().Unix() - 1,
						CpuLoad:   0.3,
					},
				},
			},
			wantErr: "could not find any available nodes",
		},
		{
			name:      "single available node",
			sortBy:    "cpuload",
			algorithm: "twochoice",
			nodes: []*livekit.Node{
				createTestNode("node1", 0.5, 3, 10, livekit.NodeState_SERVING),
			},
			wantErr:     "",
			expected:    "node1", // Should select the only available node
			notExpected: "",      // No other nodes to compare against
		},
		{
			name:      "two available nodes",
			sortBy:    "cpuload",
			algorithm: "twochoice",
			nodes: []*livekit.Node{
				createTestNode("node1", 0.8, 5, 15, livekit.NodeState_SERVING),
				createTestNode("node2", 0.3, 2, 5, livekit.NodeState_SERVING),
			},
			wantErr:     "",
			expected:    "node2", // Should select the node with lower load
			notExpected: "node1", // Should not select the node with higher load
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			selector := &AnySelector{
				SortBy:    tt.sortBy,
				Algorithm: tt.algorithm,
			}

			node, err := selector.SelectNode(tt.nodes)

			if tt.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.wantErr)
				require.Nil(t, node)
			} else {
				require.NoError(t, err)
				require.NotNil(t, node)
				require.NotEmpty(t, node.Id)

				// Verify the selected node is one of the available nodes
				found := false
				availableNodes := GetAvailableNodes(tt.nodes)
				for _, availableNode := range availableNodes {
					if availableNode.Id == node.Id {
						found = true
						break
					}
				}
				require.True(t, found, "Selected node should be one of the available nodes")

				if tt.expected != "" {
					require.Equal(t, tt.expected, node.Id, "Selected node should match expected")
				}
				if tt.notExpected != "" {
					require.NotEqual(t, tt.notExpected, node.Id, "Selected node should not match not expected")
				}
			}
		})
	}
}

func TestAnySelector_SelectNode_TwoChoice_Probabilistic_Behavior(t *testing.T) {
	// Test that two-choice algorithm favors nodes with lower metrics
	// This test runs multiple iterations to increase confidence in the probabilistic behavior
	selector := &AnySelector{
		SortBy:    "cpuload",
		Algorithm: "twochoice",
	}

	// Create nodes where node2 has significantly lower CPU load
	nodes := []*livekit.Node{
		createTestNode("node1", 0.95, 10, 20, livekit.NodeState_SERVING), // Very high load
		createTestNode("node2", 0.1, 1, 2, livekit.NodeState_SERVING),    // Low load
		createTestNode("node3", 0.5, 9, 18, livekit.NodeState_SERVING),   // Medium load
		createTestNode("node4", 0.85, 8, 16, livekit.NodeState_SERVING),  // High load
	}

	// Run multiple selections and count how often the low-load node is selected
	iterations := 1000
	lowLoadSelections := 0
	higestLoadSelections := 0

	for i := 0; i < iterations; i++ {
		node, err := selector.SelectNode(nodes)
		require.NoError(t, err)
		require.NotNil(t, node)

		if node.Id == "node2" {
			lowLoadSelections++
		}
		if node.Id == "node1" {
			higestLoadSelections++
		}
	}

	// The low-load node should be selected more often than pure random (25%)
	// Due to the two-choice algorithm favoring the better node
	selectionRate := float64(lowLoadSelections) / float64(iterations)
	require.Greater(t, selectionRate, 0.4, "Two-choice algorithm should favor the low-load node more than random selection")
	require.Equal(t, higestLoadSelections, 0, "Two-choice algorithm should never favor the highest load node")
}

func TestAnySelector_SelectNode_InvalidParameters(t *testing.T) {
	nodes := []*livekit.Node{
		createTestNode("node1", 0.5, 3, 10, livekit.NodeState_SERVING),
	}

	tests := []struct {
		name      string
		sortBy    string
		algorithm string
		wantErr   string
	}{
		{
			name:      "empty sortBy",
			sortBy:    "",
			algorithm: "twochoice",
			wantErr:   "sort by option cannot be blank",
		},
		{
			name:      "empty algorithm",
			sortBy:    "cpuload",
			algorithm: "",
			wantErr:   "node selector algorithm option cannot be blank",
		},
		{
			name:      "unknown sortBy",
			sortBy:    "invalid",
			algorithm: "twochoice",
			wantErr:   "unknown sort by option",
		},
		{
			name:      "unknown algorithm",
			sortBy:    "cpuload",
			algorithm: "invalid",
			wantErr:   "unknown node selector algorithm option",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			selector := &AnySelector{
				SortBy:    tt.sortBy,
				Algorithm: tt.algorithm,
			}

			node, err := selector.SelectNode(nodes)

			require.Error(t, err)
			require.Contains(t, err.Error(), tt.wantErr)
			require.Nil(t, node)
		})
	}
}
