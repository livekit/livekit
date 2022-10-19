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
	conf, err := NewConfig(nil, true)
	require.NoError(t, err)

	require.NoError(t, conf.unmarshalKeys("key1: secret1"))
	require.Equal(t, "secret1", conf.Keys["key1"])
}

func TestConfig_DefaultsKept(t *testing.T) {
	const content = `room:
  empty_timeout: 10`
	set := flag.NewFlagSet("test", 0)
	c := cli.NewContext(nil, set, nil)
	set.String("config-body", string(content), "")
	conf, err := NewConfig(c, true)
	require.NoError(t, err)
	require.Equal(t, true, conf.Room.AutoCreate)
	require.Equal(t, uint32(10), conf.Room.EmptyTimeout)
}

func TestConfig_UnknownKeys(t *testing.T) {
	const content = `unknown: 10
room:
  empty_timeout: 10`
	set := flag.NewFlagSet("test", 0)
	c := cli.NewContext(nil, set, nil)
	set.String("config-body", string(content), "")
	_, err := NewConfig(c, true)
	require.Error(t, err)
}
