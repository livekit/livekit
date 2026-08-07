// Copyright 2026 LiveKit, Inc.
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

package service

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/livekit-server/pkg/config"
)

func TestGetFirstKeyPairDeterministic(t *testing.T) {
	rm := &RoomManager{}
	rm.config = &config.Config{
		Keys: map[string]string{"key-b": "secret-b", "key-a": "secret-a", "key-c": "secret-c"},
	}

	first, secret, err := rm.getFirstKeyPair()
	require.NoError(t, err)
	require.Equal(t, "key-a", first)
	require.Equal(t, "secret-a", secret)

	// refreshed tokens must be signed with the same key on every call
	for i := 0; i < 100; i++ {
		k, s, err := rm.getFirstKeyPair()
		require.NoError(t, err)
		require.Equal(t, first, k)
		require.Equal(t, secret, s)
	}
}

func TestGetFirstKeyPairNoKeys(t *testing.T) {
	rm := &RoomManager{}
	rm.config = &config.Config{Keys: map[string]string{}}
	_, _, err := rm.getFirstKeyPair()
	require.Error(t, err)
}
