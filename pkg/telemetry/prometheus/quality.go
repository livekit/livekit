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
	qualityRating prometheus.Histogram
	qualityScore  prometheus.Histogram
)

func initQualityStats(nodeID string, nodeType livekit.NodeType) {
	qualityRating = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace:   livekitNamespace,
		Subsystem:   "quality",
		Name:        "rating",
		ConstLabels: prometheus.Labels{"node_id": nodeID, "node_type": nodeType.String()},
		Buckets:     []float64{0, 1, 2},
	})
	qualityScore = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace:   livekitNamespace,
		Subsystem:   "quality",
		Name:        "score",
		ConstLabels: prometheus.Labels{"node_id": nodeID, "node_type": nodeType.String()},
		Buckets:     []float64{1.0, 2.0, 2.5, 3.0, 3.25, 3.5, 3.75, 4.0, 4.25, 4.5},
	})

	prometheus.MustRegister(qualityRating)
	prometheus.MustRegister(qualityScore)
}

func RecordQuality(rating livekit.ConnectionQuality, score float32) {
	qualityRating.Observe(float64(rating))
	qualityScore.Observe(float64(score))
}
