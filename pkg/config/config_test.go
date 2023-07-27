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
	"flag"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v2"
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

func TestGeneratedFlags(t *testing.T) {
	generatedFlags, err := GenerateCLIFlags(nil, false)
	require.NoError(t, err)

	app := cli.NewApp()
	app.Name = "test"
	app.Flags = append(app.Flags, generatedFlags...)

	set := flag.NewFlagSet("test", 0)
	set.Bool("rtc.use_ice_lite", true, "")                     // bool
	set.String("redis.address", "localhost:6379", "")          // string
	set.Uint("prometheus_port", 9999, "")                      // uint32
	set.Bool("rtc.allow_tcp_fallback", true, "")               // pointer
	set.Bool("rtc.reconnect_on_publication_error", true, "")   // pointer
	set.Bool("rtc.reconnect_on_subscription_error", false, "") // pointer

	c := cli.NewContext(app, set, nil)
	conf, err := NewConfig("", true, c, nil)
	require.NoError(t, err)

	require.True(t, conf.RTC.UseICELite)
	require.Equal(t, "localhost:6379", conf.Redis.Address)
	require.Equal(t, uint32(9999), conf.PrometheusPort)

	require.NotNil(t, conf.RTC.AllowTCPFallback)
	require.True(t, *conf.RTC.AllowTCPFallback)

	require.NotNil(t, conf.RTC.ReconnectOnPublicationError)
	require.True(t, *conf.RTC.ReconnectOnPublicationError)

	require.NotNil(t, conf.RTC.ReconnectOnSubscriptionError)
	require.False(t, *conf.RTC.ReconnectOnSubscriptionError)
}
