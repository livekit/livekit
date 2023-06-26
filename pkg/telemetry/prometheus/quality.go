package prometheus

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/livekit/protocol/livekit"
)

var (
	qualityRating prometheus.Histogram
	qualityScore  prometheus.Histogram
	qualityDrop   *prometheus.CounterVec
)

func initQualityStats(nodeID string, nodeType livekit.NodeType, env string) {
	qualityRating = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace:   livekitNamespace,
		Subsystem:   "quality",
		Name:        "rating",
		ConstLabels: prometheus.Labels{"node_id": nodeID, "node_type": nodeType.String(), "env": env},
		Buckets:     []float64{0, 1, 2},
	})
	qualityScore = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace:   livekitNamespace,
		Subsystem:   "quality",
		Name:        "score",
		ConstLabels: prometheus.Labels{"node_id": nodeID, "node_type": nodeType.String(), "env": env},
		Buckets:     []float64{1.0, 2.0, 2.5, 3.0, 3.25, 3.5, 3.75, 4.0, 4.25, 4.5},
	})
	qualityDrop = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace:   livekitNamespace,
		Subsystem:   "quality",
		Name:        "drop",
		ConstLabels: prometheus.Labels{"node_id": nodeID, "node_type": nodeType.String(), "env": env},
	}, []string{"direction"})

	prometheus.MustRegister(qualityRating)
	prometheus.MustRegister(qualityScore)
	prometheus.MustRegister(qualityDrop)
}

func RecordQuality(rating livekit.ConnectionQuality, score float32, numUpDrops int, numDownDrops int) {
	qualityRating.Observe(float64(rating))
	qualityScore.Observe(float64(score))
	qualityDrop.WithLabelValues("up").Add(float64(numUpDrops))
	qualityDrop.WithLabelValues("down").Add(float64(numDownDrops))
}
