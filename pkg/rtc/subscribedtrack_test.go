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

package rtc

import (
	"sync"
	"testing"

	"github.com/pion/webrtc/v4"
	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils"

	"github.com/livekit/livekit-server/pkg/rtc/types/typesfakes"
	"github.com/livekit/livekit-server/pkg/sfu"
	"github.com/livekit/livekit-server/pkg/sfu/sfufakes"
)

// applySettings computes video layers from the settings outside settingsLock,
// so it must not read t.settings while UpdateSubscriberSettings replaces it.
// The data race this guards against is reported by `go test -race`.
func TestSubscribedTrackConcurrentSettings(t *testing.T) {
	dt, err := sfu.NewDownTrack(sfu.DownTrackParams{
		Codecs: []webrtc.RTPCodecParameters{{
			RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000},
			PayloadType:        96,
		}},
		Receiver: &sfufakes.FakeTrackReceiver{},
		Logger:   logger.GetLogger(),
		Listener: &sfufakes.FakeDownTrackListener{},
	})
	require.NoError(t, err)
	t.Cleanup(dt.Close)

	mt := &typesfakes.FakeMediaTrack{}
	mt.ToProtoReturns(&livekit.TrackInfo{})

	st := &SubscribedTrack{
		params:           SubscribedTrackParams{MediaTrack: mt},
		logger:           logger.GetLogger(),
		downTrack:        dt,
		versionGenerator: utils.NewDefaultTimedVersionGenerator(),
		debouncer:        func(f func()) { f() },
	}

	var wg sync.WaitGroup
	for i := range 4 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := range 200 {
				st.UpdateSubscriberSettings(&livekit.UpdateTrackSettings{
					Width:  uint32(100 + i*200 + j),
					Height: uint32(100 + j),
					Fps:    uint32(1 + j%30),
				}, true)
			}
		}()
		go func() {
			defer wg.Done()
			for range 200 {
				st.UpdateVideoLayer()
			}
		}()
	}
	wg.Wait()

	require.False(t, st.IsMuted())
}
