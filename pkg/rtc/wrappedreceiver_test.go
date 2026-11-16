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
	"maps"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/stretchr/testify/require"

	protoCodecs "github.com/livekit/protocol/codecs"
	"github.com/livekit/protocol/codecs/mime"
	"github.com/livekit/protocol/livekit"

	"github.com/livekit/livekit-server/pkg/sfu"
	"github.com/livekit/livekit-server/pkg/sfu/sfufakes"
)

func TestWrappedReceiverBindableCodecs(t *testing.T) {
	red, opus := protoCodecs.RedCodecParameters, protoCodecs.OpusCodecParameters

	cases := []struct {
		name      string
		encrypted bool
		upstream  []webrtc.RTPCodecParameters
		want      []mime.MimeType
	}{
		{name: "Opus, RED added", upstream: []webrtc.RTPCodecParameters{opus}, want: []mime.MimeType{mime.MimeTypeRED, mime.MimeTypeOpus}},
		{name: "encrypted Opus, added RED excluded", encrypted: true, upstream: []webrtc.RTPCodecParameters{opus}, want: []mime.MimeType{mime.MimeTypeOpus}},
		{name: "encrypted RED, added Opus excluded", encrypted: true, upstream: []webrtc.RTPCodecParameters{red}, want: []mime.MimeType{mime.MimeTypeRED}},
		{name: "encrypted RED and Opus both published", encrypted: true, upstream: []webrtc.RTPCodecParameters{red, opus}, want: []mime.MimeType{mime.MimeTypeRED, mime.MimeTypeOpus}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wr := NewWrappedReceiver(WrappedReceiverParams{UpstreamCodecs: c.upstream, IsEncrypted: c.encrypted})
			var got []mime.MimeType
			for _, codec := range wr.BindableCodecs() {
				got = append(got, mime.NormalizeMimeType(codec.MimeType))
			}
			require.Equal(t, c.want, got)
		})
	}
}

func downTrackFor(subscriberID livekit.ParticipantID) *sfufakes.FakeTrackSender {
	downTrack := &sfufakes.FakeTrackSender{}
	downTrack.SubscriberIDReturns(subscriberID)
	return downTrack
}

// downTrackRecorder is a receiver that keeps the down tracks added to it and not deleted.
func downTrackRecorder() (*sfufakes.FakeTrackReceiver, func() []livekit.ParticipantID) {
	var lock sync.Mutex
	downTracks := make(map[livekit.ParticipantID]bool)
	r := &sfufakes.FakeTrackReceiver{}
	r.AddDownTrackStub = func(t sfu.TrackSender) error {
		lock.Lock()
		defer lock.Unlock()
		downTracks[t.SubscriberID()] = true
		return nil
	}
	r.DeleteDownTrackStub = func(subscriberID livekit.ParticipantID) {
		lock.Lock()
		defer lock.Unlock()
		delete(downTracks, subscriberID)
	}
	return r, func() []livekit.ParticipantID {
		lock.Lock()
		defer lock.Unlock()
		return slices.Sorted(maps.Keys(downTracks))
	}
}

// Upgrade hands everything the dummy receiver collected to the real receiver, and a
// second Upgrade changes nothing and leaves the dummy receiver usable.
func TestDummyReceiverUpgrade(t *testing.T) {
	vp8 := webrtc.RTPCodecParameters{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: mime.MimeTypeVP8.String()}}
	d := NewDummyReceiver(&livekit.TrackInfo{Sid: "TR_dummy"}, "stream", vp8, nil)
	primaryForRed, red := d.GetPrimaryReceiverForRed(), d.GetRedReceiver()

	require.NoError(t, d.AddDownTrack(downTrackFor("PA_a")))
	require.NoError(t, d.AddDownTrack(downTrackFor("PA_gone")))
	d.DeleteDownTrack("PA_gone")
	require.NoError(t, red.AddDownTrack(downTrackFor("PA_red")))
	require.NoError(t, primaryForRed.AddDownTrack(downTrackFor("PA_primary")))
	d.AddOnReady(func() {})
	d.AddOnCodecStateChange(func(webrtc.RTPCodecParameters, sfu.ReceiverCodecState) {})
	d.SetMaxExpectedSpatialLayer(1)

	receiver, downTracks := downTrackRecorder()
	redReceiver, redDownTracks := downTrackRecorder()
	primaryReceiver, primaryDownTracks := downTrackRecorder()
	receiver.GetRedReceiverReturns(redReceiver)
	receiver.GetPrimaryReceiverForRedReturns(primaryReceiver)
	d.Upgrade(receiver)

	require.Same(t, receiver, d.Receiver())
	require.Equal(t, []livekit.ParticipantID{"PA_a"}, downTracks())
	require.Equal(t, []livekit.ParticipantID{"PA_red"}, redDownTracks())
	require.Equal(t, []livekit.ParticipantID{"PA_primary"}, primaryDownTracks())
	require.Equal(t, 1, receiver.AddOnReadyCallCount())
	require.Equal(t, 1, receiver.AddOnCodecStateChangeCallCount())
	require.Equal(t, 1, receiver.SetMaxExpectedSpatialLayerCallCount())
	require.Equal(t, int32(1), receiver.SetMaxExpectedSpatialLayerArgsForCall(0))

	other, otherDownTracks := downTrackRecorder()
	d.Upgrade(other)
	require.Same(t, receiver, d.Receiver())

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = d.AddDownTrack(downTrackFor("PA_b"))
		d.DeleteDownTrack("PA_a")
		_ = d.GetDownTracks()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("dummy receiver blocked after a second Upgrade")
	}
	require.Equal(t, []livekit.ParticipantID{"PA_b"}, downTracks())
	require.Empty(t, otherDownTracks())
	require.Zero(t, other.AddOnReadyCallCount())
}

// A down track deleted while the dummy receiver is upgraded must not end up on the
// real receiver. The race window is short, so it is run many times.
func TestDummyReceiverUpgradeRacingDeleteDownTrack(t *testing.T) {
	const subscriberID = livekit.ParticipantID("PA_sub")
	vp8 := webrtc.RTPCodecParameters{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: mime.MimeTypeVP8.String()}}

	run := func(t *testing.T, red bool) {
		for i := 0; i < 20000; i++ {
			d := NewDummyReceiver(&livekit.TrackInfo{Sid: "TR_dummy"}, "stream", vp8, nil)
			var pending sfu.TrackReceiver = d
			if red {
				pending = d.GetRedReceiver()
			}
			require.NoError(t, pending.AddDownTrack(downTrackFor(subscriberID)))

			receiver, downTracks := downTrackRecorder()
			if red {
				redReceiver, redDownTracks := downTrackRecorder()
				receiver.GetRedReceiverReturns(redReceiver)
				downTracks = redDownTracks
			}

			start := make(chan struct{})
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-start
				d.Upgrade(receiver)
			}()
			go func() {
				defer wg.Done()
				<-start
				pending.DeleteDownTrack(subscriberID)
			}()
			close(start)
			wg.Wait()

			require.Empty(t, downTracks(), "deleted down track registered on the receiver, iteration %d", i)
		}
	}

	t.Run("receiver", func(t *testing.T) { run(t, false) })
	t.Run("red", func(t *testing.T) { run(t, true) })
}
