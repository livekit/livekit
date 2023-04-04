/*
 * Copyright 2022 LiveKit, Inc
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package rtc

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/livekit"
)

func TestClientInfo_CompareVersion(t *testing.T) {
	c := ClientInfo{
		ClientInfo: &livekit.ClientInfo{
			Version: "1",
		},
	}
	require.Equal(t, 1, c.compareVersion("0.1.0"))
	require.Equal(t, 0, c.compareVersion("1.0.0"))
	require.Equal(t, -1, c.compareVersion("1.0.5"))
}

func TestClientInfo_SupportsICETCP(t *testing.T) {
	t.Run("GO SDK cannot support TCP", func(t *testing.T) {
		c := ClientInfo{
			ClientInfo: &livekit.ClientInfo{
				Sdk: livekit.ClientInfo_GO,
			},
		}
		require.False(t, c.SupportsICETCP())
	})

	t.Run("Swift SDK cannot support TCP before 1.0.5", func(t *testing.T) {
		c := ClientInfo{
			ClientInfo: &livekit.ClientInfo{
				Sdk:     livekit.ClientInfo_SWIFT,
				Version: "1.0.4",
			},
		}
		require.False(t, c.SupportsICETCP())
		c.Version = "1.0.5"
		require.True(t, c.SupportsICETCP())
	})
}
