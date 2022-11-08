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
	generatedFlags, err := GenerateCLIFlags(nil)
	require.NoError(t, err)

	app := cli.NewApp()
	app.Name = "test"
	app.Flags = append(app.Flags, generatedFlags...)

	set := flag.NewFlagSet("test", 0)
	set.Bool("rtc.use_ice_lite", true, "")            // bool
	set.String("redis.address", "localhost:6379", "") // string
	set.Uint("prometheus_port", 9999, "")             // uint32
	set.Bool("rtc.allow_tcp_fallback", true, "")      // pointer

	c := cli.NewContext(app, set, nil)
	conf, err := NewConfig("", true, c, nil)
	require.NoError(t, err)

	require.True(t, conf.RTC.UseICELite)
	require.Equal(t, "localhost:6379", conf.Redis.Address)
	require.Equal(t, uint32(9999), conf.PrometheusPort)
	require.NotNil(t, conf.RTC.AllowTCPFallback)
	require.True(t, *conf.RTC.AllowTCPFallback)
}
