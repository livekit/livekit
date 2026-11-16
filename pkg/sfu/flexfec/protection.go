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

package flexfec

import (
	"math"
)

// ProtectionMode selects how the repair rate of a protected stream is chosen.
// The modes follow libwebrtc's RtpFecProtection::Mode.
type ProtectionMode int

const (
	// ProtectionDisabled generates no repairs.
	ProtectionDisabled ProtectionMode = iota
	// ProtectionAdaptive follows observed packet loss, RTT, bits per frame and
	// resolution, using libwebrtc's hybrid NACK/FEC protection logic.
	ProtectionAdaptive
	// ProtectionFixed uses Protection.Factor regardless of network conditions.
	ProtectionFixed
	// ProtectionMinimum uses the larger of the adaptive rate and Protection.Factor.
	ProtectionMinimum
)

func (m ProtectionMode) String() string {
	switch m {
	case ProtectionDisabled:
		return "disabled"
	case ProtectionAdaptive:
		return "adaptive"
	case ProtectionFixed:
		return "fixed"
	case ProtectionMinimum:
		return "minimum"
	default:
		return "unknown"
	}
}

// Protection is a requested protection setting.
type Protection struct {
	Mode ProtectionMode
	// Factor is the Q8 repair-to-media packet ratio used by ProtectionFixed and
	// ProtectionMinimum for both delta and key frames. It is ignored otherwise.
	Factor uint8
}

func (p Protection) usesAdaptive() bool {
	return p.Mode == ProtectionAdaptive || p.Mode == ProtectionMinimum
}

// ProtectionFactorFromPercent converts a repair-to-media percentage to the Q8
// FEC rate, truncating like the publish-track presets (38/63/89 for 15/25/35).
// Values above 100 are capped.
func ProtectionFactorFromPercent(percent uint32) uint8 {
	return uint8(min(percent, MaxProtectionPercent) * 255 / 100)
}

// protectionFactorToPercent rounds to the nearest percentage, so that a
// percentage converted with ProtectionFactorFromPercent maps back to itself.
func protectionFactorToPercent(factor uint8) uint32 {
	return (uint32(factor)*100 + 127) / 255
}

// The constants and functions below port libwebrtc's VCMNackFecMethod and
// VCMLossProtectionLogic (modules/video_coding/media_opt_util.{h,cc}) as used
// by FecControllerDefault with NACK and FEC both enabled, which is always the
// case for SFU subscribers that negotiate FlexFEC.
const (
	// Max value of loss rates in the off-line model; the table has this many columns.
	packetLossMax    = 129
	fecRateTableSize = 6450

	// The SFU protects every forwarded frame, so all forwarded temporal layers
	// are accounted for as one layer.
	adaptiveNumLayers = 1
	// VCMProtectionMethod::_maxPayloadSize
	adaptiveMaxPayloadSize = 1460
	// Default VCMLossProtectionLogic resolution, used until one is known.
	adaptiveDefaultWidth  = 704
	adaptiveDefaultHeight = 576

	// kLowRttNackMs: below this RTT, NACK alone protects delta frames.
	lowRttNackMs = 20
	// Max RTT for BitRateTooLowForFec to turn off FEC.
	maxRttTurnOffFecMs = 200
	// Bytes-per-frame thresholds below which FEC is turned off.
	maxBytesPerFrameForFec     = 700
	maxBytesPerFrameForFecLow  = 400
	maxBytesPerFrameForFecHigh = 1000

	// kLossPrHistorySize and kLossPrShortFilterWinMs for the max-window loss filter.
	lossPrHistorySize       = 10
	lossPrShortFilterWinMs  = 1000
	packetsPerFrameAlpha    = 0.9999
	expFilterValueUndefined = -1.0
)

type adaptiveParams struct {
	// filtered loss, Q8
	loss        uint8
	rttMs       int64
	bitrateKbps float64
	frameRate   float64
	// average packets per delta and key frame, or expFilterValueUndefined
	packetsPerFrame    float64
	packetsPerFrameKey float64
	width              int
	height             int
}

// bitsPerFrame returns the average kbits per frame (VCMFecMethod::BitsPerFrame).
func (p *adaptiveParams) bitsPerFrame() int {
	frameRate := max(p.frameRate, 1)
	return saturateInt(p.bitrateKbps / frameRate)
}

// adaptiveProtectionFactors returns the Q8 delta and key frame protection
// factors relative to the number of media packets, which is what the encoder
// uses (VCMNackFecMethod::UpdateParameters).
func adaptiveProtectionFactors(p adaptiveParams) (delta uint8, key uint8) {
	delta, key = protectionFactor(&p)
	// Hybrid NACK/FEC: below kLowRttNackMs NACK alone recovers delta frames.
	// Above it libwebrtc keeps the full rate (its RTT adjustment is 1.0).
	if p.rttMs < lowRttNackMs {
		delta = 0
	}
	if bitRateTooLowForFec(&p) {
		delta, key = 0, 0
	}
	return convertFECRate(delta), convertFECRate(key)
}

// protectionFactor ports VCMFecMethod::ProtectionFactor. The factors are
// relative to the total number of source and repair packets.
func protectionFactor(p *adaptiveParams) (codeRateDelta uint8, codeRateKey uint8) {
	packetLoss := int(p.loss)
	if packetLoss == 0 {
		return 0, 0
	}

	// First partition protection: ~ 20%
	const firstPartitionProt = 51 // 255 * 0.20
	// Threshold on packetLoss and bitRate/frameRate (=average #packets),
	// above which protection covers at least the first partition.
	const lossThr = 0
	const packetNumThr = 1
	// Parameters for the range of the rate index of the table.
	const ratePar1 = 5
	const ratePar2 = 49

	// Spatial resolution size, relative to a reference size.
	width, height := p.width, p.height
	if width <= 0 || height <= 0 {
		width, height = adaptiveDefaultWidth, adaptiveDefaultHeight
	}
	spatialSizeToRef := float32(width*height) / float32(704*576)
	// resolnFac generally increases/decreases the FEC rate (for a fixed bit
	// rate and packet loss) based on system size, softened by an exponent < 1.
	resolnFac := 1.0 / float32(math.Pow(float64(spatialSizeToRef), 0.3))

	bitRatePerFrame := p.bitsPerFrame()

	// Average number of packets per frame (source and FEC).
	avgTotPackets := saturateUint8(1.5 + float64(bitRatePerFrame)*1000.0/(8.0*adaptiveMaxPayloadSize))

	// The table index is based on an effective rate; its range corresponds to
	// ~200k to ~8000k bps at 30 fps.
	effRateFecTable := int(saturateUint16(float64(resolnFac * float32(bitRatePerFrame))))
	rateIndexTable := max(min((effRateFecTable-ratePar1)/ratePar1, ratePar2), 0)

	// The table is defined for loss up to 50%.
	if packetLoss >= packetLossMax {
		packetLoss = packetLossMax - 1
	}

	// Protection factor for P frames.
	delta := int(fecRateTable[rateIndexTable*packetLossMax+packetLoss])
	if packetLoss > lossThr && int(avgTotPackets) > packetNumThr {
		// Set a minimum based on the first partition size.
		delta = max(delta, firstPartitionProt)
	}
	// 50% is the max protection for P frames.
	if delta >= packetLossMax {
		delta = packetLossMax - 1
	}

	// Key frames are effectively at a higher rate, so scale/boost the rate.
	packetFrameDelta := saturateUint8(0.5 + p.packetsPerFrame)
	packetFrameKey := saturateUint8(0.5 + p.packetsPerFrameKey)
	boostKey := boostCodeRateKey(packetFrameDelta, packetFrameKey)

	rateIndexTable = max(min(1+(boostKey*effRateFecTable-ratePar1)/ratePar1, ratePar2), 0)
	indexTableKey := min(rateIndexTable*packetLossMax+packetLoss, fecRateTableSize-1)

	// Protection factor for I frames.
	key := int(fecRateTable[indexTableKey])

	// Boosting for key frames (_scaleProtKey = 2.0).
	boostKeyProt := 2 * delta
	if boostKeyProt >= packetLossMax {
		boostKeyProt = packetLossMax - 1
	}

	// Key frame protection is at least the delta frame protection and at
	// least the filtered packet loss.
	key = max(packetLoss, boostKeyProt, key)
	// 50% is the max protection for I frames.
	if key >= packetLossMax {
		key = packetLossMax - 1
	}
	return uint8(delta), uint8(key)
}

// boostCodeRateKey ports VCMFecMethod::BoostCodeRateKey.
func boostCodeRateKey(packetFrameDelta uint8, packetFrameKey uint8) int {
	const boostRateKey = 2
	// Default: ratio scales the FEC protection up for I frames.
	ratio := 1
	if packetFrameDelta > 0 {
		ratio = int(packetFrameKey / packetFrameDelta)
	}
	return max(boostRateKey, ratio)
}

// bitRateTooLowForFec ports VCMNackFecMethod::BitRateTooLowForFec, below which
// FEC is turned off regardless of reported packet loss.
func bitRateTooLowForFec(p *adaptiveParams) bool {
	estimateBytesPerFrame := 1000 * p.bitsPerFrame() / 8
	maxBytesPerFrame := maxBytesPerFrameForFec
	width, height := p.width, p.height
	if width <= 0 || height <= 0 {
		width, height = adaptiveDefaultWidth, adaptiveDefaultHeight
	}
	numPixels := width * height
	if numPixels <= 352*288 {
		maxBytesPerFrame = maxBytesPerFrameForFecLow
	} else if numPixels > 640*480 {
		maxBytesPerFrame = maxBytesPerFrameForFecHigh
	}
	return estimateBytesPerFrame < maxBytesPerFrame && adaptiveNumLayers < 3 && p.rttMs < maxRttTurnOffFecMs
}

// convertFECRate ports VCMFecMethod::ConvertFECRate: the table defines rates
// relative to source plus repair packets, the encoder relative to source packets.
func convertFECRate(codeRate uint8) uint8 {
	return saturateUint8(min(255, 0.5+255.0*float64(codeRate)/float64(255-int(codeRate))))
}

type lossSample struct {
	lossPr255 uint8
	timeMs    int64
}

// lossFilter ports the max-window filter (kMaxFilter) of
// VCMLossProtectionLogic: the max loss over 1 second windows, for the last
// lossPrHistorySize seconds.
type lossFilter struct {
	history  [lossPrHistorySize]lossSample
	shortMax uint8
}

func newLossFilter() lossFilter {
	f := lossFilter{}
	for i := range f.history {
		f.history[i].timeMs = -1
	}
	return f
}

// filteredLoss records a received loss sample and returns the filtered loss.
func (f *lossFilter) filteredLoss(nowMs int64, lossPr255 uint8) uint8 {
	f.update(lossPr255, nowMs)
	return f.max(nowMs)
}

func (f *lossFilter) update(lossPr255 uint8, nowMs int64) {
	if f.history[0].timeMs >= 0 && nowMs-f.history[0].timeMs < lossPrShortFilterWinMs {
		f.shortMax = max(f.shortMax, lossPr255)
		return
	}
	// Only add a new value to the history once a second.
	if f.history[0].timeMs == -1 {
		// First, no shift.
		f.shortMax = lossPr255
	} else {
		copy(f.history[1:], f.history[:lossPrHistorySize-1])
	}
	if f.shortMax == 0 {
		f.shortMax = lossPr255
	}
	f.history[0] = lossSample{lossPr255: f.shortMax, timeMs: nowMs}
	f.shortMax = 0
}

func (f *lossFilter) max(nowMs int64) uint8 {
	maxFound := f.shortMax
	for _, s := range f.history {
		if s.timeMs == -1 || nowMs-s.timeMs > lossPrHistorySize*lossPrShortFilterWinMs {
			// This sample (and all samples after this) is too old.
			break
		}
		maxFound = max(maxFound, s.lossPr255)
	}
	return maxFound
}

// expFilter ports rtc_base ExpFilter without a max limit.
type expFilter struct {
	alpha    float64
	filtered float64
}

func newExpFilter(alpha float64) expFilter {
	return expFilter{alpha: alpha, filtered: expFilterValueUndefined}
}

func (f *expFilter) apply(exp float64, sample float64) {
	if f.filtered == expFilterValueUndefined {
		f.filtered = sample
		return
	}
	alpha := math.Pow(f.alpha, exp)
	f.filtered = alpha*f.filtered + (1-alpha)*sample
}

func saturateUint8(v float64) uint8 {
	switch {
	case math.IsNaN(v) || v <= 0:
		return 0
	case v >= math.MaxUint8:
		return math.MaxUint8
	default:
		return uint8(v)
	}
}

func saturateUint16(v float64) uint16 {
	switch {
	case math.IsNaN(v) || v <= 0:
		return 0
	case v >= math.MaxUint16:
		return math.MaxUint16
	default:
		return uint16(v)
	}
}

func saturateInt(v float64) int {
	switch {
	case math.IsNaN(v) || v <= 0:
		return 0
	case v >= math.MaxInt32:
		return math.MaxInt32
	default:
		return int(v)
	}
}
