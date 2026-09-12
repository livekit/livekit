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

	"github.com/pion/webrtc/v4"

	"github.com/livekit/protocol/livekit"

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
			encoder := flexfec.NewEncoder(uint8(codec.PayloadType), uint32(t.SSRCForwardErrorCorrection()), d.params.OnFECSent)
			encoder.SeedState(d.fecState)
			encoder.SetProtectionPercent(d.fecProtectionPercent.Load())
			d.fecEncoder.Store(encoder)
			return
		}
	}
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

// Include the nominal repair overhead in every allocator input, so layer
// selection leaves room for repair traffic. The pacer/BWE account actual bytes.
func (d *DownTrack) getLayeredBitrateWithFEC() ([]int32, Bitrates) {
	layers, bitrates := d.Receiver().GetLayeredBitrate()
	if d.fecEncoder.Load() != nil {
		percent := int64(d.fecProtectionPercent.Load())
		if percent == 0 {
			return layers, bitrates
		}
		for spatial := range bitrates {
			for temporal, bitrate := range bitrates[spatial] {
				bitrates[spatial][temporal] += bitrate * percent / 100
			}
		}
	}
	return layers, bitrates
}

// SetFECProtection updates a subscriber's video FEC preset, using the same
// 0/15/25/35 percent levels as the publish-track options. Unknown levels disable FEC.
// It can run before Bind and never enables FEC unless it was negotiated and
// permitted by the server. Changes take effect without SDP renegotiation.
func (d *DownTrack) SetFECProtection(level livekit.FECProtection) {
	var percent uint32
	switch level {
	case livekit.FECProtection_FEC_LOW:
		percent = 15
	case livekit.FECProtection_FEC_MEDIUM:
		percent = 25
	case livekit.FECProtection_FEC_HIGH:
		percent = 35
	}
	d.fecLock.Lock()
	previous := d.fecProtectionPercent.Swap(percent)
	if previous == percent {
		d.fecLock.Unlock()
		return
	}
	encoder := d.fecEncoder.Load()
	if encoder != nil {
		encoder.SetProtectionPercent(percent)
	}
	d.fecLock.Unlock()
	if encoder != nil {
		if listener := d.getStreamAllocatorListener(); listener != nil {
			listener.OnSubscriptionChanged(d)
		}
	}
}
