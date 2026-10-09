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

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/livekit-server/version"
)

func TestMain(m *testing.M) {
	if os.Getenv("LIVEKIT_TEST_RUN_MAIN") == "1" {
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestMainExitCode(t *testing.T) {
	testBinary, err := os.Executable()
	require.NoError(t, err)
	missingConfig := filepath.Join(t.TempDir(), "missing.yaml")

	for _, tt := range []struct {
		name     string
		args     []string
		exitCode int
		output   string
	}{
		{
			name:     "missing config",
			args:     []string{"--config", missingConfig, "ports"},
			exitCode: 1,
			output:   missingConfig,
		},
		{
			name:     "unknown flag",
			args:     []string{"--unknown-flag"},
			exitCode: 1,
			output:   "flag provided but not defined",
		},
		{
			name:     "invalid flag value",
			args:     []string{"--dev=invalid"},
			exitCode: 1,
			output:   "invalid value",
		},
		{
			name:   "help",
			args:   []string{"--help"},
			output: "High performance WebRTC server",
		},
		{
			name:   "version",
			args:   []string{"--version"},
			output: version.Version,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cmd := exec.CommandContext(t.Context(), testBinary, tt.args...)
			// Run the real entry point without inheriting server configuration.
			cmd.Env = []string{"LIVEKIT_TEST_RUN_MAIN=1"}
			output, err := cmd.CombinedOutput()
			if tt.exitCode == 0 {
				require.NoError(t, err, string(output))
			} else {
				var exitErr *exec.ExitError
				require.ErrorAs(t, err, &exitErr, string(output))
				require.Equal(t, tt.exitCode, exitErr.ExitCode(), string(output))
			}
			require.Contains(t, string(output), tt.output)
		})
	}
}

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
