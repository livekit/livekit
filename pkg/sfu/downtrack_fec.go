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

package sfu

import (
	"strings"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/livekit/protocol/livekit"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/sfu/flexfec"
)

// bindFEC runs under bindLock, using the complete negotiated codec list saved
// before Pion narrows the TrackLocalContext to the selected media codec.
func (d *DownTrack) bindFEC(t webrtc.TrackLocalContext) {
	d.fecLock.Lock()
	defer d.fecLock.Unlock()
	d.closeFECLocked()
	if !d.params.EnableFlexFEC || d.kind != webrtc.RTPCodecTypeVideo || t.SSRCForwardErrorCorrection() == 0 {
		return
	}
	for _, codec := range d.negotiatedCodecParameters {
		if strings.EqualFold(codec.MimeType, webrtc.MimeTypeFlexFEC03) {
			encoder := flexfec.NewEncoder(uint8(codec.PayloadType), uint32(t.SSRCForwardErrorCorrection()), d.onFECSent)
			encoder.SeedState(d.fecState)
			encoder.SetProtection(d.fecProtection)
			if d.forwarder != nil {
				encoder.SetMeasurementLayer(d.forwarder.CurrentLayer().Spatial)
			}
			d.setFECNotifiedOverhead(encoder.Overhead())
			d.fecEncoder.Store(encoder)
			return
		}
	}
}

// getActiveFECEncoder returns the encoder for forwarded media, or nil while
// protection is disabled, the default for negotiated FEC, so that packets do
// not take the encoder lock without generating repairs. Adaptive protection
// then sees no media rates; it starts from the loss history and measures
// rates again once enabled.
func (d *DownTrack) getActiveFECEncoder() *flexfec.Encoder {
	if encoder := d.fecEncoder.Load(); encoder != nil && encoder.Enabled() {
		return encoder
	}
	return nil
}

func (d *DownTrack) closeFEC() {
	d.fecLock.Lock()
	defer d.fecLock.Unlock()
	d.closeFECLocked()
}

func (d *DownTrack) closeFECLocked() {
	if encoder := d.fecEncoder.Swap(nil); encoder != nil {
		encoder.Close()
		// Close first so queued media cannot advance the sequence after it is saved.
		d.fecState = encoder.GetState()
	}
}

func (d *DownTrack) getFECState() flexfec.EncoderState {
	d.fecLock.Lock()
	defer d.fecLock.Unlock()
	if encoder := d.fecEncoder.Load(); encoder != nil {
		return encoder.GetState()
	}
	return d.fecState
}

func (d *DownTrack) seedFECState(state flexfec.EncoderState) {
	if state.SSRC == 0 {
		return
	}
	d.fecLock.Lock()
	defer d.fecLock.Unlock()
	d.fecState = state
	if encoder := d.fecEncoder.Load(); encoder != nil {
		encoder.SeedState(state)
	}
}

func (d *DownTrack) onFECSent(packets int, bytes int) {
	if d.params.OnFECSent != nil {
		d.params.OnFECSent(packets, bytes)
	}
	// Account repairs separately from media, like the receiving side.
	if d.rtpStats != nil {
		d.rtpStats.UpdateFEC(uint64(packets), uint64(bytes), 0, 0)
	}
	if encoder := d.fecEncoder.Load(); encoder != nil {
		d.maybeNotifyFECOverhead(encoder)
	}
}

// setFECMeasurementLayer attributes repair overhead measurements to the
// spatial layer now forwarded. Called when forwarding starts, resumes or
// switches layers.
func (d *DownTrack) setFECMeasurementLayer() {
	encoder := d.fecEncoder.Load()
	if encoder == nil {
		return
	}
	encoder.SetMeasurementLayer(d.forwarder.CurrentLayer().Spatial)
	d.maybeNotifyFECOverhead(encoder)
}

// Reallocate only when the reserved overhead moves by at least this many
// percentage points, so that per-second measurements and adaptive protection
// following every receiver report do not re-run allocation and flip layers
// near a capacity boundary, each switch costing a key frame.
const fecOverheadNotifyThreshold = 5

func (d *DownTrack) setFECNotifiedOverhead(overhead flexfec.OverheadEstimate) {
	d.fecNotifyLock.Lock()
	d.fecNotifiedOverhead = overhead
	d.fecNotifyLock.Unlock()
}

// maybeNotifyFECOverhead lets the allocator reserve a changed repair overhead.
func (d *DownTrack) maybeNotifyFECOverhead(encoder *flexfec.Encoder) {
	overhead := encoder.Overhead()
	d.fecNotifyLock.Lock()
	notified := d.fecNotifiedOverhead
	changed := overhead.Layer != notified.Layer ||
		fecOverheadChanged(notified.ConfiguredPercent, overhead.ConfiguredPercent) ||
		fecOverheadChanged(notified.LayerPercent, overhead.LayerPercent)
	if changed {
		d.fecNotifiedOverhead = overhead
	}
	d.fecNotifyLock.Unlock()
	if changed {
		if listener := d.getStreamAllocatorListener(); listener != nil {
			listener.OnSubscriptionChanged(d)
		}
	}
}

// fecOverheadChanged reports a change past the threshold; turning repair on
// or off always counts.
func fecOverheadChanged(from uint32, to uint32) bool {
	if (from == 0) != (to == 0) {
		return true
	}
	if from > to {
		from, to = to, from
	}
	return to-from >= fecOverheadNotifyThreshold
}

// Reserve repair overhead in every allocator input: the measured overhead on
// the forwarded spatial layer, where small frames can require much more than
// the nominal preset, and the configured rate on the other layers, whose
// overhead is not measured. The pacer/BWE account actual bytes, including all
// repair headers.
func (d *DownTrack) getLayeredBitrateWithFEC() ([]int32, Bitrates) {
	layers, bitrates := d.Receiver().GetLayeredBitrate()
	if encoder := d.fecEncoder.Load(); encoder != nil {
		overhead := encoder.Overhead()
		for spatial := range bitrates {
			percent := int64(overhead.PercentForLayer(int32(spatial)))
			if percent == 0 {
				continue
			}
			for temporal, bitrate := range bitrates[spatial] {
				bitrates[spatial][temporal] += bitrate * percent / 100
			}
		}
	}
	return layers, bitrates
}

// Repair-to-media packet ratios of the protection levels, matching the
// publish-track options. FEC_LEVEL_UNSPECIFIED is the server default, low.
var fecLevelPercent = map[livekit.FECProtectionLevel]uint32{
	livekit.FECProtectionLevel_FEC_LEVEL_UNSPECIFIED: 15,
	livekit.FECProtectionLevel_FEC_LEVEL_LOW:         15,
	livekit.FECProtectionLevel_FEC_LEVEL_MEDIUM:      25,
	livekit.FECProtectionLevel_FEC_LEVEL_HIGH:        35,
}

// fecProtectionFromProto maps a subscriber's setting to encoder protection.
// nil and unknown modes disable FEC; unknown levels are treated as unspecified.
func fecProtectionFromProto(fec *livekit.FECProtection) flexfec.Protection {
	var mode flexfec.ProtectionMode
	switch fec.GetMode() {
	case livekit.FECProtectionMode_FEC_MODE_ADAPTIVE:
		return flexfec.Protection{Mode: flexfec.ProtectionAdaptive}
	case livekit.FECProtectionMode_FEC_MODE_FIXED:
		mode = flexfec.ProtectionFixed
	case livekit.FECProtectionMode_FEC_MODE_MINIMUM:
		mode = flexfec.ProtectionMinimum
	default:
		return flexfec.Protection{}
	}
	percent, ok := fecLevelPercent[fec.GetLevel()]
	if !ok {
		percent = fecLevelPercent[livekit.FECProtectionLevel_FEC_LEVEL_UNSPECIFIED]
	}
	return flexfec.Protection{Mode: mode, Factor: flexfec.ProtectionFactorFromPercent(percent)}
}

// SetFECProtection updates a subscriber's video FEC protection. Fixed and
// minimum levels use the same 15/25/35 percent ratios as the publish-track
// options; adaptive protection follows receiver reports like libwebrtc.
// It can run before Bind and never enables FEC unless it was negotiated and
// permitted by the server. Changes take effect without SDP renegotiation.
func (d *DownTrack) SetFECProtection(fec *livekit.FECProtection) {
	protection := fecProtectionFromProto(fec)
	d.fecLock.Lock()
	if d.fecProtection == protection {
		d.fecLock.Unlock()
		return
	}
	d.fecProtection = protection
	encoder := d.fecEncoder.Load()
	if encoder != nil {
		encoder.SetProtection(protection)
		d.setFECNotifiedOverhead(encoder.Overhead())
	}
	d.fecLock.Unlock()
	if encoder != nil {
		if listener := d.getStreamAllocatorListener(); listener != nil {
			listener.OnSubscriptionChanged(d)
		}
	}
}

// onFECReceiverReport feeds a loss sample aggregated from reception reports
// of the media SSRC to adaptive and minimum protection, and lets the allocator
// reserve a changed overhead.
func (d *DownTrack) onFECReceiverReport(fractionLost uint8, rttMs uint32) {
	encoder := d.fecEncoder.Load()
	if encoder == nil {
		return
	}
	width, height := d.getForwardedVideoDimensions()
	encoder.OnReceiverReport(flexfec.NetworkStats{
		FractionLost: fractionLost,
		RTT:          time.Duration(rttMs) * time.Millisecond,
		Width:        width,
		Height:       height,
	})
	d.maybeNotifyFECOverhead(encoder)
}

// getForwardedVideoDimensions returns the published dimensions of the layer
// currently forwarded, or zeros when they are unknown.
func (d *DownTrack) getForwardedVideoDimensions() (uint32, uint32) {
	spatial := d.forwarder.CurrentLayer().Spatial
	if spatial < 0 {
		return 0, 0
	}
	mimeType := d.Mime()
	trackInfo := d.Receiver().TrackInfo()
	quality := buffer.SpatialLayerToVideoQuality(mimeType, spatial, trackInfo)
	for _, layer := range buffer.GetVideoLayersForMimeType(mimeType, trackInfo) {
		if layer.GetQuality() == quality {
			return layer.GetWidth(), layer.GetHeight()
		}
	}
	return 0, 0
}
