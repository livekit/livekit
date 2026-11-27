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

package prometheus

import (
	"testing"

	promclient "github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/livekit"
)

func TestKeyFrameMetrics(t *testing.T) {
	require.NoError(t, Init("test", livekit.NodeType_SERVER))

	incomingBefore := counterValue(t, promKeyFrameTotal.WithLabelValues(string(Incoming)))
	outgoingBefore := counterValue(t, promKeyFrameTotal.WithLabelValues(string(Outgoing)))

	IncrementKeyFrames(Incoming)
	IncrementKeyFrames(Outgoing)
	IncrementKeyFrames(Outgoing)

	require.Equal(t, incomingBefore+1, counterValue(t, promKeyFrameTotal.WithLabelValues(string(Incoming))))
	require.Equal(t, outgoingBefore+2, counterValue(t, promKeyFrameTotal.WithLabelValues(string(Outgoing))))
}

func counterValue(t *testing.T, counter promclient.Counter) float64 {
	t.Helper()

	metric := &dto.Metric{}
	require.NoError(t, counter.Write(metric))
	return metric.GetCounter().GetValue()
}

func TestForwardProcessingPerWriteMetrics(t *testing.T) {
	require.NoError(t, Init("test", livekit.NodeType_SERVER))

	countBefore, sumBefore := histogramValue(t, promForwardProcessingPerWriteHist)
	RecordForwardProcessingPerWriteSample(3_500)
	RecordForwardProcessingPerWriteSample(4_500)
	count, sum := histogramValue(t, promForwardProcessingPerWriteHist)
	require.Equal(t, countBefore+2, count)
	require.Equal(t, sumBefore+8_000, sum)

	RecordForwardProcessingPerWrite(4_000)
	require.Equal(t, uint32(4_000), forwardProcessingPerWrite.Load())
}

func histogramValue(t *testing.T, histogram promclient.Histogram) (uint64, float64) {
	t.Helper()

	metric := &dto.Metric{}
	require.NoError(t, histogram.Write(metric))
	return metric.GetHistogram().GetSampleCount(), metric.GetHistogram().GetSampleSum()
}

func TestFECMetricsCountNodeTraffic(t *testing.T) {
	require.NoError(t, Init("test", livekit.NodeType_SERVER))

	sentBefore := counterValue(t, promFECDownstreamSent)
	bytesSentBefore := counterValue(t, promFECDownstreamBytesSent)
	receivedBefore := counterValue(t, promFECUpstreamReceived)
	packetsOutBefore, bytesOutBefore := packetsOut.Load(), bytesOut.Load()
	packetsInBefore, bytesInBefore := packetsIn.Load(), bytesIn.Load()

	RecordFECDownstream(2, 300)
	RecordFECUpstream(3, 1, 0, 400)

	require.Equal(t, sentBefore+2, counterValue(t, promFECDownstreamSent))
	require.Equal(t, bytesSentBefore+300, counterValue(t, promFECDownstreamBytesSent))
	require.Equal(t, receivedBefore+3, counterValue(t, promFECUpstreamReceived))
	require.Equal(t, packetsOutBefore+2, packetsOut.Load(), "repairs sent are node traffic")
	require.Equal(t, bytesOutBefore+300, bytesOut.Load())
	require.Equal(t, packetsInBefore, packetsIn.Load(), "recovered media already counts as ingress")
	require.Equal(t, bytesInBefore, bytesIn.Load())
}
