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

package prometheus

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/livekit/protocol/livekit"
)

var (
	refCounts *prometheus.GaugeVec
)

func initDebugStats(nodeID string, nodeType livekit.NodeType) {
	refCounts = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace:   livekitNamespace,
		Subsystem:   "debug",
		Name:        "ref_count",
		ConstLabels: prometheus.Labels{"node_id": nodeID, "node_type": nodeType.String()},
	}, []string{"referrer"})

	prometheus.MustRegister(refCounts)
}

func AddRef(referrer string, n int) {
	refCounts.WithLabelValues(referrer).Add(float64(n))
}
