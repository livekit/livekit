package main

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
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
