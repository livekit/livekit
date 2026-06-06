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

package config

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v3"

	"github.com/livekit/livekit-server/pkg/config/configtest"
)

func TestConfig_UnmarshalKeys(t *testing.T) {
	conf, err := NewConfig("", true, nil, nil)
	require.NoError(t, err)

	require.NoError(t, conf.unmarshalKeys("key1: secret1"))
	require.Equal(t, "secret1", conf.Keys["key1"])
}

func TestConfig_DefaultsKept(t *testing.T) {
	const content = `room:
  empty_timeout: 10`
	conf, err := NewConfig(content, true, nil, nil)
	require.NoError(t, err)
	require.Equal(t, true, conf.Room.AutoCreate)
	require.Equal(t, uint32(10), conf.Room.EmptyTimeout)
}

func TestConfig_UnknownKeys(t *testing.T) {
	const content = `unknown: 10
room:
  empty_timeout: 10`
	_, err := NewConfig(content, true, nil, nil)
	require.Error(t, err)
}

func TestFlexFECConfigEnabled(t *testing.T) {
	require.False(t, FlexFECConfig{}.Enabled(), "disabled by default")
	require.False(t, FlexFECConfig{Publisher: true}.Enabled(), "payload type is required")
	require.False(t, FlexFECConfig{Subscriber: true}.Enabled(), "payload type is required")
	require.True(t, FlexFECConfig{Publisher: true, PayloadType: 49}.Enabled())
	require.True(t, FlexFECConfig{Subscriber: true, PayloadType: 49}.Enabled())
	require.True(t, FlexFECConfig{Publisher: true, Subscriber: true, PayloadType: 49}.Enabled())
}

func TestFlexFECConfigEncoderParams(t *testing.T) {
	for _, tc := range []struct {
		name     string
		conf     FlexFECConfig
		expected [2]uint32
	}{
		{
			name:     "defaults",
			conf:     FlexFECConfig{},
			expected: [2]uint32{10, 2},
		},
		{
			name:     "custom",
			conf:     FlexFECConfig{NumMediaPackets: 8, NumFecPackets: 3},
			expected: [2]uint32{8, 3},
		},
		{
			name:     "media_clamped_to_minimum",
			conf:     FlexFECConfig{NumMediaPackets: 1, NumFecPackets: 1},
			expected: [2]uint32{2, 1},
		},
		{
			name:     "fec_clamped_below_media",
			conf:     FlexFECConfig{NumMediaPackets: 5, NumFecPackets: 5},
			expected: [2]uint32{5, 4},
		},
		{
			name:     "fec_above_media_clamped",
			conf:     FlexFECConfig{NumMediaPackets: 4, NumFecPackets: 10},
			expected: [2]uint32{4, 3},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			numMediaPackets, numFecPackets := tc.conf.EncoderParams()
			require.Equal(t, tc.expected[0], numMediaPackets)
			require.Equal(t, tc.expected[1], numFecPackets)
		})
	}
}

func TestGeneratedFlags(t *testing.T) {
	generatedFlags, err := GenerateCLIFlags(nil, false)
	require.NoError(t, err)

	c := &cli.Command{}
	c.Name = "test"
	c.Flags = append(c.Flags, generatedFlags...)

	c.Set("rtc.use_ice_lite", "true")
	c.Set("redis.address", "localhost:6379")
	c.Set("prometheus.port", "9999")
	c.Set("rtc.allow_tcp_fallback", "true")
	c.Set("rtc.reconnect_on_publication_error", "true")
	c.Set("rtc.reconnect_on_subscription_error", "false")

	conf, err := NewConfig("", true, c, nil)
	require.NoError(t, err)

	require.True(t, conf.RTC.UseICELite)
	require.Equal(t, "localhost:6379", conf.Redis.Address)
	require.Equal(t, uint32(9999), conf.Prometheus.Port)

	require.NotNil(t, conf.RTC.AllowTCPFallback)
	require.True(t, *conf.RTC.AllowTCPFallback)

	require.NotNil(t, conf.RTC.ReconnectOnPublicationError)
	require.True(t, *conf.RTC.ReconnectOnPublicationError)

	require.NotNil(t, conf.RTC.ReconnectOnSubscriptionError)
	require.False(t, *conf.RTC.ReconnectOnSubscriptionError)
}

func TestYAMLTag(t *testing.T) {
	require.NoError(t, configtest.CheckYAMLTags(Config{}))
}
