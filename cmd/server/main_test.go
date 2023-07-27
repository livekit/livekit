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
