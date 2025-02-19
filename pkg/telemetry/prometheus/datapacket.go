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
	"slices"
	"strings"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/livekit/protocol/livekit"
)

var (
	promDataPacketStreamLabels    = []string{"type", "mime_type"}
	promDataPacketStreamMimeTypes = []string{"text", "image", "application", "audio", "video"}

	promDataPacketStreamDestCount *prometheus.HistogramVec
	promDataPacketStreamSize      *prometheus.HistogramVec
)

func initDataPacketStats(nodeID string, nodeType livekit.NodeType) {
	promDataPacketStreamDestCount = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace:   livekitNamespace,
		Subsystem:   "datapacket_stream",
		Name:        "dest_count",
		ConstLabels: prometheus.Labels{"node_id": nodeID, "node_type": nodeType.String()},
		Buckets:     []float64{1, 2, 3, 4, 5, 10, 15, 25, 50},
	}, promDataPacketStreamLabels)
	promDataPacketStreamSize = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace:   livekitNamespace,
		Subsystem:   "datapacket_stream",
		Name:        "bytes",
		ConstLabels: prometheus.Labels{"node_id": nodeID, "node_type": nodeType.String()},
		Buckets:     []float64{128, 512, 2048, 8192, 32768, 131072, 524288, 2097152, 8388608, 33554432},
	}, promDataPacketStreamLabels)

	prometheus.MustRegister(promDataPacketStreamDestCount)
	prometheus.MustRegister(promDataPacketStreamSize)
}

func RecordDataPacketStream(h *livekit.DataStream_Header, destCount int) {
	streamType := "unknown"
	switch h.ContentHeader.(type) {
	case *livekit.DataStream_Header_TextHeader:
		streamType = "text"
	case *livekit.DataStream_Header_ByteHeader:
		streamType = "bytes"
	}

	mimeType := strings.ToLower(h.MimeType)
	if i := strings.IndexByte(mimeType, '/'); i != -1 {
		mimeType = mimeType[:i]
	}
	if !slices.Contains(promDataPacketStreamMimeTypes, mimeType) {
		mimeType = "unknown"
	}

	promDataPacketStreamDestCount.WithLabelValues(streamType, mimeType).Observe(float64(destCount))
	if h.TotalLength != nil {
		promDataPacketStreamSize.WithLabelValues(streamType, mimeType).Observe(float64(*h.TotalLength))
	}
}
