// Copyright 2026 LiveKit, Inc.
// SPDX-License-Identifier: Apache-2.0

package moq

import (
	"testing"

	"github.com/livekit/protocol/codecs/mime"
	"github.com/livekit/protocol/livekit"
	"github.com/stretchr/testify/require"

	"github.com/livekit/livekit-server/pkg/moq/media"
)

func TestVideoCatalogMetadata(t *testing.T) {
	info := &livekit.TrackInfo{Width: 1920, Height: 1080}
	var single media.CatalogTrack
	setVideoCatalogMetadata(&single, info, mime.MimeTypeVP8, nil)
	require.Equal(t, media.CatalogTrack{Width: 1920, Height: 1080}, single)

	info.Codecs = []*livekit.SimulcastCodecInfo{{MimeType: "video/VP8", Layers: []*livekit.VideoLayer{
		{SpatialLayer: 1, Width: 1920, Height: 1080},
		{SpatialLayer: 0, Width: 640, Height: 360, Bitrate: 320000},
	}}}
	var base media.CatalogTrack
	setVideoCatalogMetadata(&base, info, mime.MimeTypeVP8, []float32{12, 24})
	require.Equal(t, media.CatalogTrack{Width: 640, Height: 360, Bitrate: 320000, Framerate: 24}, base)
}

func TestReceiverRestartPreservesMoQSubscription(t *testing.T) {
	tap := newMoQTap(&moqRendition{codec: mime.MimeTypeVP8, catalog: media.CatalogTrack{Role: media.RoleVideo}}, "subscriber")
	old := tap.asm
	tap.started, tap.needKey = true, false
	tap.frames <- moqSample{}

	tap.ReceiverRestart(nil)
	require.False(t, tap.IsClosed())
	require.True(t, tap.waitingForKeyframe())
	require.False(t, tap.started)
	require.Empty(t, tap.frames)
	require.NotSame(t, old, tap.asm)
}
