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

package streamallocator

import (
	"strings"
	"sync"
	"testing"

	"github.com/pion/webrtc/v4"
	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/codecs/mime"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/sfu"
	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/sfu/bwe"
	"github.com/livekit/livekit-server/pkg/sfu/pacer"
	"github.com/livekit/livekit-server/pkg/sfu/sfufakes"
)

// floorNullBWE completes bwe.NullBWE (pattern: pkg/sfu/downtrack_downstream_integration_test.go).
type floorNullBWE struct {
	*bwe.NullBWE
}

func (floorNullBWE) Type() bwe.BWEType { return bwe.BWETypeNone }

var (
	// unique rates per layer, so BandwidthRequested identifies the target layer.
	// f-T0 (400) is cheaper than h-T2 (450).
	floorBrsCheapF = sfu.Bitrates{
		{50, 80, 100, 0},
		{200, 300, 450, 0},
		{400, 700, 1000, 0},
	}
	// T3 present, so f-T2 stays deficient
	floorBrsLadder = sfu.Bitrates{
		{100, 150, 200, 250},
		{300, 400, 500, 600},
		{1000, 1100, 1200, 1300},
	}
)

type floorFixture struct {
	t *testing.T
	s *StreamAllocator
	p pacer.Pacer
}

// newFloorFixture: a StreamAllocator (queue not started; handlers driven directly), allow_pause=false as on staging.
func newFloorFixture(t *testing.T, floor int32, l logger.Logger) *floorFixture {
	t.Helper()

	if l == nil {
		l = logger.GetLogger()
	}
	p := pacer.NewPassThrough(logger.GetLogger(), floorNullBWE{NullBWE: &bwe.NullBWE{}})
	t.Cleanup(p.Stop)

	cfg := DefaultStreamAllocatorConfig
	cfg.CameraMinTemporalLayer = floor
	s := NewStreamAllocator(StreamAllocatorParams{
		Config: cfg,
		BWE:    floorNullBWE{NullBWE: &bwe.NullBWE{}},
		Pacer:  p,
		Logger: l,
	}, true, false)
	return &floorFixture{t: t, s: s, p: p}
}

// addTrack adds a real VP8 simulcast DownTrack over a FakeTrackReceiver.
func (c *floorFixture) addTrack(id string, source livekit.TrackSource, brs sfu.Bitrates) *Track {
	c.t.Helper()

	codecParams := webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000},
		PayloadType:        96,
	}
	rcv := &sfufakes.FakeTrackReceiver{}
	rcv.TrackIDReturns(livekit.TrackID(id))
	rcv.StreamIDReturns("fake-stream-" + id)
	rcv.CodecReturns(codecParams)
	rcv.MimeReturns(mime.NormalizeMimeType(codecParams.MimeType))
	rcv.VideoLayerModeReturns(livekit.VideoLayer_ONE_SPATIAL_LAYER_PER_STREAM)
	rcv.TrackInfoReturns(&livekit.TrackInfo{})
	rcv.GetLayeredBitrateReturns(nil, brs)

	dt, err := sfu.NewDownTrack(sfu.DownTrackParams{
		Codecs:        []webrtc.RTPCodecParameters{codecParams},
		Receiver:      rcv,
		BufferFactory: buffer.NewFactoryOfBufferFactory(500, 500).CreateBufferFactory(),
		Pacer:         c.p,
		Logger:        logger.GetLogger(),
		Listener:      &sfufakes.FakeDownTrackListener{},
		StreamID:      "fake-stream-" + id,
		SubID:         "fake-sub",
		MaxTrack:      500,
	})
	require.NoError(c.t, err)
	// harness trap: the layer mode comes from ReceiverRestart, not from the constructor
	dt.ReceiverRestart(rcv)
	dt.SetMaxSpatialLayer(buffer.DefaultMaxLayerSpatial)
	dt.SetMaxTemporalLayer(buffer.DefaultMaxLayerTemporal)
	dt.UpTrackMaxPublishedLayerChange(buffer.DefaultMaxLayerSpatial)
	dt.UpTrackMaxTemporalLayerSeenChange(buffer.DefaultMaxLayerTemporal)

	c.s.AddTrack(dt, AddTrackParams{Source: source, IsMultiLayered: true})
	for _, track := range c.s.getVideoTracks() {
		if track.ID() == livekit.TrackID(id) {
			return track
		}
	}
	require.FailNow(c.t, "track not added", id)
	return nil
}

func (c *floorFixture) congested(estimate int64) {
	c.s.handleSignalCongestionStateChange(Event{
		congestionStateChangeData: congestionStateChangeData{
			fromState:                         bwe.CongestionStateNone,
			toState:                           bwe.CongestionStateCongested,
			estimatedAvailableChannelCapacity: estimate,
		},
	})
}

// floorTarget maps the track's BandwidthRequested back to the (unique) layer of brs.
func floorTarget(t *testing.T, track *Track, brs sfu.Bitrates) buffer.VideoLayer {
	t.Helper()
	requested := track.BandwidthRequested()
	for s := range brs {
		for tl := range brs[s] {
			if brs[s][tl] == requested {
				return buffer.VideoLayer{Spatial: int32(s), Temporal: int32(tl)}
			}
		}
	}
	require.Failf(t, "no layer", "BandwidthRequested %d not in bitrates", requested)
	return buffer.InvalidLayer
}

// allocateAllTracks under congestion: an estimate that fits f-T0 but not h-T2.
func TestCameraFloorAllocateAllTracks(t *testing.T) {
	fT0 := buffer.VideoLayer{Spatial: 2, Temporal: 0}
	hT1 := buffer.VideoLayer{Spatial: 1, Temporal: 1}
	cases := []struct {
		name   string
		source livekit.TrackSource
		floor  int32
		want   buffer.VideoLayer
	}{
		{"camera key absent", livekit.TrackSource_CAMERA, 0, fT0},
		{"camera floor 1", livekit.TrackSource_CAMERA, 1, hT1},
		{"screen share key absent", livekit.TrackSource_SCREEN_SHARE, 0, fT0},
		{"screen share floor 1 same as key absent", livekit.TrackSource_SCREEN_SHARE, 1, fT0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fx := newFloorFixture(t, c.floor, nil)
			track := fx.addTrack("t1", c.source, floorBrsCheapF)
			fx.congested(420)
			require.Equal(t, c.want, floorTarget(t, track, floorBrsCheapF))
		})
	}
}

// Two cameras compete: B needs bits while A streams f-T2 with no headroom, so A offers its best weighted transition.
func TestCameraFloorBestWeightedTwoCameras(t *testing.T) {
	qT0 := buffer.VideoLayer{Spatial: 0, Temporal: 0}
	fT0 := buffer.VideoLayer{Spatial: 2, Temporal: 0}
	fT1 := buffer.VideoLayer{Spatial: 2, Temporal: 1}
	fT2 := buffer.VideoLayer{Spatial: 2, Temporal: 2}
	for _, c := range []struct {
		floor int32
		wantA buffer.VideoLayer
	}{
		{0, fT0},
		{1, fT1},
	} {
		fx := newFloorFixture(t, c.floor, nil)
		a := fx.addTrack("a", livekit.TrackSource_CAMERA, floorBrsLadder)
		fx.congested(1200)
		require.Equal(t, fT2, floorTarget(t, a, floorBrsLadder), "precondition: A streams f-T2")
		require.Equal(t, streamAllocatorStateDeficient, fx.s.state, "precondition: deficient")

		// B is not streaming (a new DownTrack targets its max layer until allocated)
		b := fx.addTrack("b", livekit.TrackSource_CAMERA, floorBrsLadder)
		b.Pause()
		require.Zero(t, b.BandwidthRequested(), "precondition: B not streaming")
		fx.s.allocateTrack(b)
		require.Equal(t, c.wantA, floorTarget(t, a, floorBrsLadder), "floor %d", c.floor)
		require.Equal(t, qT0, floorTarget(t, b, floorBrsLadder), "floor %d", c.floor)
	}
}

type floorRecordingLogger struct {
	logger.Logger

	mu    sync.Mutex
	infos []string
}

func (l *floorRecordingLogger) Infow(msg string, keysAndValues ...any) {
	l.mu.Lock()
	l.infos = append(l.infos, msg)
	l.mu.Unlock()
	l.Logger.Infow(msg, keysAndValues...)
}

func TestCameraFloorConfiguredLog(t *testing.T) {
	for _, c := range []struct {
		floor int32
		want  []string
	}{
		{0, nil},
		{1, []string{"camera temporal floor configured floor=1"}},
	} {
		l := &floorRecordingLogger{Logger: logger.GetLogger()}
		newFloorFixture(t, c.floor, l)

		var got []string
		for _, msg := range l.infos {
			if strings.HasPrefix(msg, "camera temporal floor") {
				got = append(got, msg)
			}
		}
		require.Equal(t, c.want, got, "floor %d", c.floor)
	}
}
