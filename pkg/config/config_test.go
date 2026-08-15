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
	"os"
	"path/filepath"
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

func TestConfig_TURNURLOptions(t *testing.T) {
	conf, err := NewConfig("", true, nil, nil)
	require.NoError(t, err)
	require.False(t, conf.TURN.AdvertiseTLSPort)
	require.False(t, conf.TURN.UDPUseDomain)

	const content = `turn:
  advertise_tls_port: true
  udp_use_domain: true`
	conf, err = NewConfig(content, true, nil, nil)
	require.NoError(t, err)
	require.True(t, conf.TURN.AdvertiseTLSPort)
	require.True(t, conf.TURN.UDPUseDomain)
}

func TestConfig_SignalMessageSizeLimitDefaults(t *testing.T) {
	conf, err := NewConfig("", true, nil, nil)
	require.NoError(t, err)
	// both default to 2 MiB when not specified
	require.Equal(t, int64(2<<20), conf.Limit.SignalMessageSizeLimit)
	require.Equal(t, int64(2<<20), conf.Limit.AgentSignalMessageSizeLimit)
}

func TestConfig_SignalMessageSizeLimitOverride(t *testing.T) {
	const content = `limit:
  signal_message_size_limit: 1024
  agent_signal_message_size_limit: 0`
	conf, err := NewConfig(content, true, nil, nil)
	require.NoError(t, err)
	require.Equal(t, int64(1024), conf.Limit.SignalMessageSizeLimit)
	// 0 explicitly disables the limit
	require.Equal(t, int64(0), conf.Limit.AgentSignalMessageSizeLimit)
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

func TestClampTURNTTLSeconds(t *testing.T) {
	cases := []struct {
		name    string
		in      int
		want    int
		changed bool
	}{
		{"negative falls back to default", -1, DefaultTURNTTLSeconds, true},
		{"large negative falls back to default", -1 << 40, DefaultTURNTTLSeconds, true},
		{"zero falls back to default", 0, DefaultTURNTTLSeconds, true},
		{"in range preserved", 600, 600, false},
		{"max preserved", TURNMaxTTLSeconds, TURNMaxTTLSeconds, false},
		{"over max clamps to max", TURNMaxTTLSeconds + 1, TURNMaxTTLSeconds, true},
		{"overflowing value clamps to max", 1<<62 + 1, TURNMaxTTLSeconds, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, changed := ClampTURNTTLSeconds(c.in)
			require.Equal(t, c.want, got)
			require.Equal(t, c.changed, changed)
		})
	}
}

func TestNormalizeTURNTTLs(t *testing.T) {
	conf, err := NewConfig("", true, nil, nil)
	require.NoError(t, err)

	conf.TURN.TTLSeconds = -5
	conf.RTC.TURNServers = []TURNServer{
		{Host: "over", TTL: TURNMaxTTLSeconds + 100},
		{Host: "negative", TTL: -1},
		{Host: "default", TTL: 0},
		{Host: "ok", TTL: 600},
	}

	conf.NormalizeTURNTTLs()

	// embedded TTL <= 0 falls back to the 5m default
	require.Equal(t, DefaultTURNTTLSeconds, conf.TURN.TTLSeconds)
	// external TTLs: only the upper bound is capped at load; 0/negative keep their
	// "use the default" meaning and are resolved when credentials are generated
	require.Equal(t, TURNMaxTTLSeconds, conf.RTC.TURNServers[0].TTL)
	require.Equal(t, -1, conf.RTC.TURNServers[1].TTL)
	require.Equal(t, 0, conf.RTC.TURNServers[2].TTL)
	require.Equal(t, 600, conf.RTC.TURNServers[3].TTL)
}

func TestNewConfigNormalizesTURNTTL(t *testing.T) {
	const content = `turn:
  ttl_seconds: -10`
	conf, err := NewConfig(content, true, nil, nil)
	require.NoError(t, err)
	require.Equal(t, DefaultTURNTTLSeconds, conf.TURN.TTLSeconds)
}

func writeSecretFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "secret")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

func TestLoadTURNSecrets(t *testing.T) {
	t.Run("empty secret file is rejected", func(t *testing.T) {
		conf := &Config{}
		conf.RTC.TURNServers = []TURNServer{{Host: "h", SecretFile: writeSecretFile(t, "")}}
		require.ErrorIs(t, conf.LoadTURNSecrets(), ErrTURNSecretEmpty)
	})

	t.Run("whitespace-only secret file is rejected", func(t *testing.T) {
		conf := &Config{}
		conf.RTC.TURNServers = []TURNServer{{Host: "h", SecretFile: writeSecretFile(t, "  \n\t ")}}
		require.ErrorIs(t, conf.LoadTURNSecrets(), ErrTURNSecretEmpty)
	})

	t.Run("valid secret file is loaded and trimmed", func(t *testing.T) {
		conf := &Config{}
		conf.RTC.TURNServers = []TURNServer{{Host: "h", SecretFile: writeSecretFile(t, "  topsecret\n")}}
		require.NoError(t, conf.LoadTURNSecrets())
		require.Equal(t, "topsecret", conf.RTC.TURNServers[0].Secret)
	})

	t.Run("inline whitespace secret without static creds is rejected", func(t *testing.T) {
		conf := &Config{}
		conf.RTC.TURNServers = []TURNServer{{Host: "h", Secret: "   "}}
		require.ErrorIs(t, conf.LoadTURNSecrets(), ErrTURNServerNoCredentials)
	})

	t.Run("no credentials at all is rejected", func(t *testing.T) {
		conf := &Config{}
		conf.RTC.TURNServers = []TURNServer{{Host: "h"}}
		require.ErrorIs(t, conf.LoadTURNSecrets(), ErrTURNServerNoCredentials)
	})

	t.Run("static credentials are accepted", func(t *testing.T) {
		conf := &Config{}
		conf.RTC.TURNServers = []TURNServer{{Host: "h", Username: "u", Credential: "c"}}
		require.NoError(t, conf.LoadTURNSecrets())
	})

	t.Run("partial static credentials are rejected", func(t *testing.T) {
		conf := &Config{}
		conf.RTC.TURNServers = []TURNServer{{Host: "h", Username: "u"}}
		require.ErrorIs(t, conf.LoadTURNSecrets(), ErrTURNServerNoCredentials)
	})

	t.Run("inline secret takes precedence and is trimmed", func(t *testing.T) {
		conf := &Config{}
		conf.RTC.TURNServers = []TURNServer{{Host: "h", Secret: " inline ", SecretFile: writeSecretFile(t, "fromfile")}}
		require.NoError(t, conf.LoadTURNSecrets())
		require.Equal(t, "inline", conf.RTC.TURNServers[0].Secret)
	})

	t.Run("blank inline secret falls back to secret file", func(t *testing.T) {
		conf := &Config{}
		conf.RTC.TURNServers = []TURNServer{{Host: "h", Secret: "   ", SecretFile: writeSecretFile(t, "fromfile")}}
		require.NoError(t, conf.LoadTURNSecrets())
		require.Equal(t, "fromfile", conf.RTC.TURNServers[0].Secret)
	})
}
