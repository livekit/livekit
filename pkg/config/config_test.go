package config

import (
	"flag"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v2"
)

type testStruct struct {
	configFileName string
	configBody     string

	expectedError      error
	expectedConfigBody string
}

func TestGetConfigString(t *testing.T) {
	tests := []testStruct{
		{"", "", nil, ""},
		{"", "configBody", nil, "configBody"},
		{"file", "configBody", nil, "configBody"},
		{"file", "", nil, "fileContent"},
	}
	for _, test := range tests {
		func() {
			writeConfigFile(test, t)
			defer os.Remove(test.configFileName)

			configBody, err := getConfigString(test.configFileName, test.configBody)
			require.Equal(t, test.expectedError, err)
			require.Equal(t, test.expectedConfigBody, configBody)
		}()
	}
}

func TestShouldReturnErrorIfConfigFileDoesNotExist(t *testing.T) {
	configBody, err := getConfigString("notExistingFile", "")
	require.Error(t, err)
	require.Empty(t, configBody)
}

func writeConfigFile(test testStruct, t *testing.T) {
	if test.configFileName != "" {
		d1 := []byte(test.expectedConfigBody)
		err := os.WriteFile(test.configFileName, d1, 0o644)
		require.NoError(t, err)
	}
}

func TestConfig_UnmarshalKeys(t *testing.T) {
	conf, err := NewConfig(nil, nil, true)
	require.NoError(t, err)

	require.NoError(t, conf.unmarshalKeys("key1: secret1"))
	require.Equal(t, "secret1", conf.Keys["key1"])
}

func TestConfig_DefaultsKept(t *testing.T) {
	const content = `room:
  empty_timeout: 10`
	app := cli.NewApp()
	set := flag.NewFlagSet("test", 0)
	c := cli.NewContext(app, set, nil)
	set.String("config-body", string(content), "")
	conf, err := NewConfig(c, nil, true)
	require.NoError(t, err)
	require.Equal(t, true, conf.Room.AutoCreate)
	require.Equal(t, uint32(10), conf.Room.EmptyTimeout)
}

func TestConfig_UnknownKeys(t *testing.T) {
	const content = `unknown: 10
room:
  empty_timeout: 10`
	app := cli.NewApp()
	set := flag.NewFlagSet("test", 0)
	c := cli.NewContext(app, set, nil)
	set.String("config-body", string(content), "")
	_, err := NewConfig(c, nil, true)
	require.Error(t, err)
}

func TestGeneratedFlags(t *testing.T) {
	generatedFlags, err := GenerateCLIFlags(nil)
	require.NoError(t, err)

	app := cli.NewApp()
	app.Flags = append(app.Flags, generatedFlags...)

	set := flag.NewFlagSet("test", 0)
	set.Bool("rtc.use_ice_lite", true, "")            // bool
	set.String("redis.address", "localhost:6379", "") // string
	set.Uint("prometheus_port", 9999, "")             // uint32
	set.Bool("rtc.allow_tcp_fallback", true, "")      // pointer

	c := cli.NewContext(app, set, nil)
	conf, err := NewConfig(c, nil, true)
	require.NoError(t, err)

	require.True(t, conf.RTC.UseICELite)
	require.Equal(t, "localhost:6379", conf.Redis.Address)
	require.Equal(t, uint32(9999), conf.PrometheusPort)
	require.NotNil(t, conf.RTC.AllowTCPFallback)
	require.True(t, *conf.RTC.AllowTCPFallback)
}
