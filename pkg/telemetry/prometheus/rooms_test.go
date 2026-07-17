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

func TestPacketTrailerTrackMetrics(t *testing.T) {
	require.NoError(t, Init("test", livekit.NodeType_SERVER))

	track := &livekit.TrackInfo{
		Type: livekit.TrackType_VIDEO,
		PacketTrailerFeatures: []livekit.PacketTrailerFeature{
			livekit.PacketTrailerFeature_PTF_USER_TIMESTAMP,
			livekit.PacketTrailerFeature_PTF_USER_TIMESTAMP,
			livekit.PacketTrailerFeature_PTF_FRAME_ID,
			livekit.PacketTrailerFeature_PTF_USER_DATA,
			livekit.PacketTrailerFeature(99),
			livekit.PacketTrailerFeature(100),
		},
	}

	trailerBefore := gaugeValue(t, promTrackPacketTrailer)
	timestampBefore := gaugeValue(t, promTrackPacketTrailerByFeature.WithLabelValues("PTF_USER_TIMESTAMP"))
	frameIDBefore := gaugeValue(t, promTrackPacketTrailerByFeature.WithLabelValues("PTF_FRAME_ID"))
	userDataBefore := gaugeValue(t, promTrackPacketTrailerByFeature.WithLabelValues("PTF_USER_DATA"))
	unknownBefore := gaugeValue(t, promTrackPacketTrailerByFeature.WithLabelValues("UNKNOWN"))

	AddPacketTrailerTrack(track)
	require.Equal(t, trailerBefore+1, gaugeValue(t, promTrackPacketTrailer))
	require.Equal(t, timestampBefore+1, gaugeValue(t, promTrackPacketTrailerByFeature.WithLabelValues("PTF_USER_TIMESTAMP")))
	require.Equal(t, frameIDBefore+1, gaugeValue(t, promTrackPacketTrailerByFeature.WithLabelValues("PTF_FRAME_ID")))
	require.Equal(t, userDataBefore+1, gaugeValue(t, promTrackPacketTrailerByFeature.WithLabelValues("PTF_USER_DATA")))
	require.Equal(t, unknownBefore+1, gaugeValue(t, promTrackPacketTrailerByFeature.WithLabelValues("UNKNOWN")))

	SubPacketTrailerTrack(track)
	require.Equal(t, trailerBefore, gaugeValue(t, promTrackPacketTrailer))
	require.Equal(t, timestampBefore, gaugeValue(t, promTrackPacketTrailerByFeature.WithLabelValues("PTF_USER_TIMESTAMP")))
	require.Equal(t, frameIDBefore, gaugeValue(t, promTrackPacketTrailerByFeature.WithLabelValues("PTF_FRAME_ID")))
	require.Equal(t, userDataBefore, gaugeValue(t, promTrackPacketTrailerByFeature.WithLabelValues("PTF_USER_DATA")))
	require.Equal(t, unknownBefore, gaugeValue(t, promTrackPacketTrailerByFeature.WithLabelValues("UNKNOWN")))
}

func TestPacketTrailerTrackMetricsIgnoreAudio(t *testing.T) {
	require.NoError(t, Init("test", livekit.NodeType_SERVER))

	before := gaugeValue(t, promTrackPacketTrailer)
	AddPacketTrailerTrack(&livekit.TrackInfo{
		Type:                  livekit.TrackType_AUDIO,
		PacketTrailerFeatures: []livekit.PacketTrailerFeature{livekit.PacketTrailerFeature_PTF_USER_DATA},
	})
	require.Equal(t, before, gaugeValue(t, promTrackPacketTrailer))
}

func gaugeValue(t *testing.T, gauge promclient.Gauge) float64 {
	t.Helper()

	metric := &dto.Metric{}
	require.NoError(t, gauge.Write(metric))
	return metric.GetGauge().GetValue()
}
