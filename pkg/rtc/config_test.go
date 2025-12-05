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

package rtc

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/livekit-server/pkg/config"
)

func TestWebRTCConfig_ForceRelay(t *testing.T) {
	tests := []struct {
		name          string
		forceRelay    *bool
		expectedValue *bool
	}{
		{
			name:          "ForceRelay enabled",
			forceRelay:    func() *bool { b := true; return &b }(),
			expectedValue: func() *bool { b := true; return &b }(),
		},
		{
			name:          "ForceRelay disabled",
			forceRelay:    func() *bool { b := false; return &b }(),
			expectedValue: func() *bool { b := false; return &b }(),
		},
		{
			name:          "ForceRelay not set",
			forceRelay:    nil,
			expectedValue: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conf := &config.Config{
				RTC: config.RTCConfig{
					ForceRelay: tt.forceRelay,
				},
			}

			webRTCConfig, err := NewWebRTCConfig(conf)
			require.NoError(t, err)

			if tt.expectedValue == nil {
				require.Nil(t, webRTCConfig.ForceRelay)
			} else {
				require.NotNil(t, webRTCConfig.ForceRelay)
				require.Equal(t, *tt.expectedValue, *webRTCConfig.ForceRelay)
			}
		})
	}
}
