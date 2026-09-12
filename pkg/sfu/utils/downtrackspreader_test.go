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

package utils

import (
	"testing"

	"github.com/livekit/protocol/livekit"
	"github.com/stretchr/testify/require"
)

type testSender struct {
	subscriberID livekit.ParticipantID
}

func (s testSender) SubscriberID() livekit.ParticipantID {
	return s.subscriberID
}

func TestDownTrackSpreaderResetRemainsReusable(t *testing.T) {
	spreader := NewDownTrackSpreader[testSender](DownTrackSpreaderParams{})
	first := testSender{subscriberID: "first"}
	second := testSender{subscriberID: "second"}

	spreader.Store(first)
	require.Equal(t, []testSender{first}, spreader.ResetAndGetDownTracks())
	require.Zero(t, spreader.DownTrackCount())

	spreader.Store(second)
	require.Equal(t, []testSender{second}, spreader.GetDownTracks())
}

func TestDownTrackSpreaderRejectsStoreAfterClose(t *testing.T) {
	spreader := NewDownTrackSpreader[testSender](DownTrackSpreaderParams{})
	first := testSender{subscriberID: "first"}
	late := testSender{subscriberID: "late"}

	spreader.Store(first)
	require.Equal(t, []testSender{first}, spreader.CloseAndGetDownTracks())
	require.Zero(t, spreader.DownTrackCount())

	require.False(t, spreader.TryStore(late))
	require.Zero(t, spreader.DownTrackCount())
}
