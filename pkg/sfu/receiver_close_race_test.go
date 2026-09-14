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

package sfu_test

import (
	"testing"

	"github.com/pion/webrtc/v4"
	"github.com/stretchr/testify/require"

	"github.com/livekit/livekit-server/pkg/sfu"
	"github.com/livekit/livekit-server/pkg/sfu/sfufakes"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
)

func TestReceiverBaseAddDownTrackReturnsClosedWhenCloseWinsRace(t *testing.T) {
	receiver := sfu.NewReceiverBase(
		sfu.ReceiverBaseParams{
			TrackID:  "track",
			StreamID: "stream",
			Kind:     webrtc.RTPCodecTypeAudio,
			Codec: webrtc.RTPCodecParameters{
				RTPCodecCapability: webrtc.RTPCodecCapability{
					MimeType:  webrtc.MimeTypeOpus,
					ClockRate: 48000,
					Channels:  2,
				},
				PayloadType: 111,
			},
			Logger: logger.GetLogger(),
		},
		&livekit.TrackInfo{
			Sid:    "track",
			Type:   livekit.TrackType_AUDIO,
			Source: livekit.TrackSource_MICROPHONE,
		},
		sfu.ReceiverCodecStateNormal,
	)

	track := &sfufakes.FakeTrackSender{}
	track.SubscriberIDReturns("subscriber")

	enteredAdd := make(chan struct{})
	resumeAdd := make(chan struct{})
	track.UpTrackMaxPublishedLayerChangeCalls(func(int32) {
		close(enteredAdd)
		<-resumeAdd
	})

	errCh := make(chan error, 1)
	go func() {
		errCh <- receiver.AddDownTrack(track)
	}()

	// AddDownTrack has already passed its initial IsClosed check and is now
	// paused immediately before the eventual Store.
	<-enteredAdd

	// Close terminally drains/closes the spreader before AddDownTrack resumes.
	receiver.Close("test", false)
	close(resumeAdd)

	err := <-errCh
	require.ErrorIs(t, err, sfu.ErrReceiverClosed)
	require.True(t, receiver.IsClosed())
	require.Empty(t, receiver.GetDownTracks())
}
