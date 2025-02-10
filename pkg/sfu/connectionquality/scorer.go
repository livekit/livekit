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

package connectionquality

import (
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils"
	"go.uber.org/zap/zapcore"
)

const (
	MaxMOS = float32(4.5)
	MinMOS = float32(1.0)

	cMaxScore = float64(100.0)
	cMinScore = float64(30.0)

	cIncreaseFactor = float64(0.4) // slower increase, i. e. when score is recovering move up slower -> conservative
	cDecreaseFactor = float64(0.8) // faster decrease, i. e. when score is dropping move down faster -> aggressive to be responsive to quality drops

	cDistanceWeight = float64(35.0) // each spatial layer missed drops a quality level

	cUnmuteTimeThreshold = float64(0.5)

	cPPSQuantization         = float64(2)
	cPPSMinReadings          = 10
	cModeCalculationInterval = 2 * time.Minute
)

var (
	qualityTransitionScore = map[livekit.ConnectionQuality]float64{
		livekit.ConnectionQuality_GOOD: 80,
		livekit.ConnectionQuality_POOR: 40,
		livekit.ConnectionQuality_LOST: 20,
	}
)

// ------------------------------------------

type windowStat struct {
	startedAt         time.Time
	duration          time.Duration
	packets           uint32
	packetsPadding    uint32
	packetsLost       uint32
	packetsMissing    uint32
	packetsOutOfOrder uint32
	bytes             uint64
	rttMax            uint32
	jitterMax         float64
	lastRTCPAt        time.Time
}

func (w *windowStat) calculatePacketScore(aplw float64, includeRTT bool, includeJitter bool) float64 {
	// this is based on simplified E-model based on packet loss, rtt, jitter as
	// outlined at https://www.pingman.com/kb/article/how-is-mos-calculated-in-pingplotter-pro-50.html.
	effectiveDelay := 0.0
	// discount the dependent factors if dependency indicated.
	// for example,
	// 1. in the up stream, RTT cannot be measured without RTCP-XR, it is using down stream RTT.
	// 2. in the down stream, up stream jitter affects it. although jitter can be adjusted to account for up stream
	//    jitter, this lever can be used to discount jitter in scoring.
	if includeRTT {
		effectiveDelay += float64(w.rttMax) / 2.0
	}
	if includeJitter {
		effectiveDelay += (w.jitterMax * 2.0) / 1000.0
	}
	delayEffect := effectiveDelay / 40.0
	if effectiveDelay > 160.0 {
		delayEffect = (effectiveDelay - 120.0) / 10.0
	}

	// discount out-of-order packets from loss to deal with a scenario like
	// 1. up stream has loss
	// 2. down stream forwards with loss/hole in sequence number
	// 3. down stream client reports a certain number of loss via RTCP RR
	// 4. while processing that RTCP RR, up stream could have retransmitted missing packets
	// 5. those retransmitted packets are forwarded,
	//    - server's view: it has forwarded those packets
	//    - client's view: it had not seen those packets when sending RTCP RR
	//    so those retransmitted packets appear like down stream loss to server.
	//
	// retransmitted packets would have arrived out-of-order. So, discounting them
	// will account for it.
	//
	// Note that packets can arrive out-of-order in the upstream during regular
	// streaming as well, i. e. without loss + NACK + retransmit. Those will be
	// discounted too. And that will skew the real loss. For example, let
	// us say that 40 out of 100 packets were reported lost by down stream.
	// These could be real losses. In the same window, 40 packets could have been
	// delivered out-of-order by the up stream, thus cancelling out the real loss.
	// But, those situations should be rare and is a compromise for not letting
	// up stream loss penalise down stream.
	actualLost := w.packetsLost - w.packetsMissing - w.packetsOutOfOrder
	if int32(actualLost) < 0 {
		actualLost = 0
	}

	var lossEffect float64
	if w.packets+w.packetsPadding > 0 {
		lossEffect = float64(actualLost) * 100.0 / float64(w.packets+w.packetsPadding)
	}
	lossEffect *= aplw

	score := cMaxScore - delayEffect - lossEffect
	if score < 0.0 {
		score = 0.0
	}

	return score
}

func (w *windowStat) calculateBitrateScore(expectedBits int64, isEnabled bool) float64 {
	if expectedBits == 0 || !isEnabled {
		// unsupported mode OR all layers stopped
		return cMaxScore
	}

	var score float64
	if w.bytes != 0 {
		// using the ratio of expectedBits / actualBits
		// the quality inflection points are approximately
		// GOOD at ~2.7x, POOR at ~20.1x
		score = cMaxScore - 20*math.Log(float64(expectedBits)/float64(w.bytes*8))
		if score > cMaxScore {
			score = cMaxScore
		}
		if score < 0.0 {
			score = 0.0
		}
	}

	return score
}

func (w *windowStat) String() string {
	return fmt.Sprintf("start: %+v, dur: %+v, p: %d, pp: %d, pl: %d, pm: %d, pooo: %d, b: %d, rtt: %d, jitter: %0.2f, lastRTCP: %+v",
		w.startedAt,
		w.duration,
		w.packets,
		w.packetsPadding,
		w.packetsLost,
		w.packetsMissing,
		w.packetsOutOfOrder,
		w.bytes,
		w.rttMax,
		w.jitterMax,
		w.lastRTCPAt,
	)
}

func (w *windowStat) MarshalLogObject(e zapcore.ObjectEncoder) error {
	if w == nil {
		return nil
	}

	e.AddTime("startedAt", w.startedAt)
	e.AddString("duration", w.duration.String())
	e.AddUint32("packets", w.packets)
	e.AddUint32("packetsPadding", w.packetsPadding)
	e.AddUint32("packetsLost", w.packetsLost)
	e.AddUint32("packetsMissing", w.packetsMissing)
	e.AddUint32("packetsOutOfOrder", w.packetsOutOfOrder)
	e.AddUint64("bytes", w.bytes)
	e.AddUint32("rttMax", w.rttMax)
	e.AddFloat64("jitterMax", w.jitterMax)
	e.AddTime("lastRTCPAt", w.lastRTCPAt)
	return nil
}

// ------------------------------------------

type qualityScorerParams struct {
	IncludeRTT         bool
	IncludeJitter      bool
	EnableBitrateScore bool
	Logger             logger.Logger
}

type qualityScorer struct {
	params qualityScorerParams

	lock         sync.RWMutex
	lastUpdateAt time.Time

	packetLossWeight float64

	score float64
	stat  windowStat

	mutedAt   time.Time
	unmutedAt time.Time

	layerMutedAt   time.Time
	layerUnmutedAt time.Time

	pausedAt  time.Time
	resumedAt time.Time

	ppsHistogram     [250]int
	numPPSReadings   int
	ppsMode          int
	modeCalculatedAt time.Time

	aggregateBitrate *utils.TimedAggregator[int64]
	layerDistance    *utils.TimedAggregator[float64]
}

func newQualityScorer(params qualityScorerParams) *qualityScorer {
	return &qualityScorer{
		params: params,
		score:  cMaxScore,
		aggregateBitrate: utils.NewTimedAggregator[int64](utils.TimedAggregatorParams{
			CapNegativeValues: true,
		}),
		layerDistance: utils.NewTimedAggregator[float64](utils.TimedAggregatorParams{
			CapNegativeValues: true,
		}),
		modeCalculatedAt: time.Now().Add(-cModeCalculationInterval),
	}
}

func (q *qualityScorer) startAtLocked(packetLossWeight float64, at time.Time) {
	q.packetLossWeight = packetLossWeight
	q.lastUpdateAt = at
}

func (q *qualityScorer) StartAt(packetLossWeight float64, at time.Time) {
	q.lock.Lock()
	defer q.lock.Unlock()

	q.startAtLocked(packetLossWeight, at)
}

func (q *qualityScorer) Start(packetLossWeight float64) {
	q.lock.Lock()
	defer q.lock.Unlock()

	q.startAtLocked(packetLossWeight, time.Now())
}

func (q *qualityScorer) UpdatePacketLossWeight(packetLossWeight float64) {
	q.lock.Lock()
	defer q.lock.Unlock()

	q.packetLossWeight = packetLossWeight
}

func (q *qualityScorer) updateMuteAtLocked(isMuted bool, at time.Time) {
	if isMuted {
		q.mutedAt = at
		// muting when LOST should not push quality to EXCELLENT
		if q.score != qualityTransitionScore[livekit.ConnectionQuality_LOST] {
			q.score = cMaxScore
		}
	} else {
		q.unmutedAt = at
	}
}

func (q *qualityScorer) UpdateMuteAt(isMuted bool, at time.Time) {
	q.lock.Lock()
	defer q.lock.Unlock()

	q.updateMuteAtLocked(isMuted, at)
}

func (q *qualityScorer) UpdateMute(isMuted bool) {
	q.lock.Lock()
	defer q.lock.Unlock()

	q.updateMuteAtLocked(isMuted, time.Now())
}

func (q *qualityScorer) addBitrateTransitionAtLocked(bitrate int64, at time.Time) {
	q.aggregateBitrate.AddSampleAt(bitrate, at)
}

func (q *qualityScorer) AddBitrateTransitionAt(bitrate int64, at time.Time) {
	q.lock.Lock()
	defer q.lock.Unlock()

	q.addBitrateTransitionAtLocked(bitrate, at)
}

func (q *qualityScorer) AddBitrateTransition(bitrate int64) {
	q.lock.Lock()
	defer q.lock.Unlock()

	q.addBitrateTransitionAtLocked(bitrate, time.Now())
}

func (q *qualityScorer) updateLayerMuteAtLocked(isMuted bool, at time.Time) {
	if isMuted {
		if !q.isLayerMuted() {
			q.aggregateBitrate.Reset()
			q.layerDistance.Reset()
			q.layerMutedAt = at
			q.score = cMaxScore
		}
	} else {
		if q.isLayerMuted() {
			q.layerUnmutedAt = at
		}
	}
}

func (q *qualityScorer) UpdateLayerMuteAt(isMuted bool, at time.Time) {
	q.lock.Lock()
	defer q.lock.Unlock()

	q.updateLayerMuteAtLocked(isMuted, at)
}

func (q *qualityScorer) UpdateLayerMute(isMuted bool) {
	q.lock.Lock()
	defer q.lock.Unlock()

	q.updateLayerMuteAtLocked(isMuted, time.Now())
}

func (q *qualityScorer) updatePauseAtLocked(isPaused bool, at time.Time) {
	if isPaused {
		if !q.isPaused() {
			q.aggregateBitrate.Reset()
			q.layerDistance.Reset()
			q.pausedAt = at
			q.score = cMinScore
		}
	} else {
		if q.isPaused() {
			q.resumedAt = at
		}
	}
}

func (q *qualityScorer) UpdatePauseAt(isPaused bool, at time.Time) {
	q.lock.Lock()
	defer q.lock.Unlock()

	q.updatePauseAtLocked(isPaused, at)
}

func (q *qualityScorer) UpdatePause(isPaused bool) {
	q.lock.Lock()
	defer q.lock.Unlock()

	q.updatePauseAtLocked(isPaused, time.Now())
}

func (q *qualityScorer) addLayerTransitionAtLocked(distance float64, at time.Time) {
	q.layerDistance.AddSampleAt(distance, at)
}

func (q *qualityScorer) AddLayerTransitionAt(distance float64, at time.Time) {
	q.lock.Lock()
	defer q.lock.Unlock()

	q.addLayerTransitionAtLocked(distance, at)
}

func (q *qualityScorer) AddLayerTransition(distance float64) {
	q.lock.Lock()
	defer q.lock.Unlock()

	q.addLayerTransitionAtLocked(distance, time.Now())
}

func (q *qualityScorer) updateAtLocked(stat *windowStat, at time.Time) {
	// always update transitions
	expectedBits, _, err := q.aggregateBitrate.GetAggregateAndRestartAt(at)
	if err != nil {
		q.params.Logger.Warnw("error getting expected bitrate", err)
	}
	expectedDistance, err := q.layerDistance.GetAverageAndRestartAt(at)
	if err != nil {
		q.params.Logger.Warnw("error getting expected distance", err)
	}

	// nothing to do when muted or not unmuted for long enough
	// NOTE: it is possible that unmute -> mute -> unmute transition happens in the
	//       same analysis window. On a transition to mute, quality is immediately moved
	//       EXCELLENT for responsiveness. On an unmute, the entire window data is
	//       considered (as long as enough time has passed since unmute).
	//
	//       Similarly, when paused (possibly due to congestion), score is immediately
	//       set to cMinScore for responsiveness. The layer transition is reset.
	//       On a resume, quality climbs back up using normal operation.
	if q.isMuted() || !q.isUnmutedEnough(at) || q.isLayerMuted() || q.isPaused() {
		q.lastUpdateAt = at
		return
	}

	aplw := q.getAdjustedPacketLossWeight(stat)
	reason := "none"
	var score, packetScore, bitrateScore, layerScore float64
	if stat.packets+stat.packetsPadding == 0 {
		if !stat.lastRTCPAt.IsZero() && at.Sub(stat.lastRTCPAt) > stat.duration {
			reason = "rtcp"
			score = qualityTransitionScore[livekit.ConnectionQuality_LOST]
		} else {
			reason = "dry"
			score = qualityTransitionScore[livekit.ConnectionQuality_POOR]
		}
	} else {
		packetScore = stat.calculatePacketScore(aplw, q.params.IncludeRTT, q.params.IncludeJitter)
		bitrateScore = stat.calculateBitrateScore(expectedBits, q.params.EnableBitrateScore)
		layerScore = math.Max(math.Min(cMaxScore, cMaxScore-(expectedDistance*cDistanceWeight)), 0.0)

		minScore := math.Min(packetScore, bitrateScore)
		minScore = math.Min(minScore, layerScore)

		switch {
		case packetScore == minScore:
			reason = "packet"
			score = packetScore

		case bitrateScore == minScore:
			reason = "bitrate"
			score = bitrateScore

		case layerScore == minScore:
			reason = "layer"
			score = layerScore
		}

		factor := cIncreaseFactor
		if score < q.score {
			factor = cDecreaseFactor
		}
		score = factor*score + (1.0-factor)*q.score
		if score < cMinScore {
			// lower bound to prevent score from becoming very small values due to extreme conditions.
			// Without a lower bound, it can get so low that it takes a long time to climb back to
			// better quality even under excellent conditions.
			score = cMinScore
		}
	}

	prevCQ := scoreToConnectionQuality(q.score)
	currCQ := scoreToConnectionQuality(score)
	ulgr := q.params.Logger.WithUnlikelyValues(
		"reason", reason,
		"prevScore", q.score,
		"prevQuality", prevCQ,
		"prevStat", &q.stat,
		"score", score,
		"packetScore", packetScore,
		"layerScore", layerScore,
		"bitrateScore", bitrateScore,
		"quality", currCQ,
		"stat", stat,
		"packetLossWeight", q.packetLossWeight,
		"adjustedPacketLossWeight", aplw,
		"modePPS", q.ppsMode*int(cPPSQuantization),
		"expectedBits", expectedBits,
		"expectedDistance", expectedDistance,
	)
	switch {
	case utils.IsConnectionQualityLower(prevCQ, currCQ):
		ulgr.Debugw("quality drop")
	case utils.IsConnectionQualityHigher(prevCQ, currCQ):
		ulgr.Debugw("quality rise")
	default:
		packets := stat.packets + stat.packetsPadding
		if packets != 0 && (stat.packetsLost*100/packets) > 10 {
			ulgr.Debugw("quality hold - high loss")
		}
	}

	q.score = score
	q.stat = *stat
	q.lastUpdateAt = at
}

func (q *qualityScorer) UpdateAt(stat *windowStat, at time.Time) {
	q.lock.Lock()
	defer q.lock.Unlock()

	q.updateAtLocked(stat, at)
}

func (q *qualityScorer) Update(stat *windowStat) {
	q.lock.Lock()
	defer q.lock.Unlock()

	q.updateAtLocked(stat, time.Now())
}

func (q *qualityScorer) isMuted() bool {
	return !q.mutedAt.IsZero() && (q.unmutedAt.IsZero() || q.mutedAt.After(q.unmutedAt))
}

func (q *qualityScorer) isUnmutedEnough(at time.Time) bool {
	var sinceUnmute time.Duration
	if q.unmutedAt.IsZero() {
		sinceUnmute = at.Sub(q.lastUpdateAt)
	} else {
		sinceUnmute = at.Sub(q.unmutedAt)
	}

	var sinceLayerUnmute time.Duration
	if q.layerUnmutedAt.IsZero() {
		sinceLayerUnmute = at.Sub(q.lastUpdateAt)
	} else {
		sinceLayerUnmute = at.Sub(q.layerUnmutedAt)
	}

	validDuration := sinceUnmute
	if sinceLayerUnmute < validDuration {
		validDuration = sinceLayerUnmute
	}

	sinceLastUpdate := at.Sub(q.lastUpdateAt)

	return validDuration.Seconds()/sinceLastUpdate.Seconds() > cUnmuteTimeThreshold
}

func (q *qualityScorer) isLayerMuted() bool {
	return !q.layerMutedAt.IsZero() && (q.layerUnmutedAt.IsZero() || q.layerMutedAt.After(q.layerUnmutedAt))
}

func (q *qualityScorer) isPaused() bool {
	return !q.pausedAt.IsZero() && (q.resumedAt.IsZero() || q.pausedAt.After(q.resumedAt))
}

func (q *qualityScorer) getAdjustedPacketLossWeight(stat *windowStat) float64 {
	if stat == nil || stat.duration <= 0 {
		return q.packetLossWeight
	}

	// packet loss is weighted by comparing against mode of packet rate seen.
	// this is to handle situations like DTX in audio and variable bit rate tracks like screen share.
	// and the effect of loss is not pronounced in those scenarios (audio silence, static screen share).
	// for example, DTX typically uses only 5% of packets of full packet rate. at that rate,
	// packet loss weight is reduced to ~22% of configured weight (i. e. sqrt(0.05) * configured weight)
	pps := float64(stat.packets) / stat.duration.Seconds()
	ppsQuantized := int(pps/cPPSQuantization + 0.5)
	if ppsQuantized < len(q.ppsHistogram)-1 {
		q.ppsHistogram[ppsQuantized]++
	} else {
		q.ppsHistogram[len(q.ppsHistogram)-1]++
	}
	q.numPPSReadings++

	// calculate mode sparingly, do it under the following conditions
	//  1. minimum number of readings available (AND)
	//  2. enough time has elapsed since last calculation
	if q.numPPSReadings > cPPSMinReadings && time.Since(q.modeCalculatedAt) > cModeCalculationInterval {
		q.ppsMode = 0
		for i := 0; i < len(q.ppsHistogram); i++ {
			if q.ppsHistogram[i] > q.ppsMode {
				q.ppsMode = i
			}
		}
		q.modeCalculatedAt = time.Now()
		q.params.Logger.Debugw("updating pps mode", "expected", stat.packets, "duration", stat.duration.Seconds(), "pps", pps, "ppsMode", q.ppsMode)
	}

	if q.ppsMode == 0 || q.ppsMode == len(q.ppsHistogram)-1 {
		return q.packetLossWeight
	}

	packetRatio := pps / (float64(q.ppsMode) * cPPSQuantization)
	if packetRatio > 1.0 {
		packetRatio = 1.0
	}
	return math.Sqrt(packetRatio) * q.packetLossWeight
}

func (q *qualityScorer) GetScoreAndQuality() (float32, livekit.ConnectionQuality) {
	q.lock.RLock()
	defer q.lock.RUnlock()

	return float32(q.score), scoreToConnectionQuality(q.score)
}

func (q *qualityScorer) GetMOSAndQuality() (float32, livekit.ConnectionQuality) {
	q.lock.RLock()
	defer q.lock.RUnlock()

	return scoreToMOS(q.score), scoreToConnectionQuality(q.score)
}

// ------------------------------------------

func scoreToConnectionQuality(score float64) livekit.ConnectionQuality {
	// R-factor -> livekit.ConnectionQuality scale mapping roughly based on
	// https://www.itu.int/ITU-T/2005-2008/com12/emodelv1/tut.htm
	//
	// As there are only three levels in livekit.ConnectionQuality scale,
	// using a larger range for middling quality. Empirical evidence suggests
	// that a score of 60 does not correspond to `POOR` quality. Repair
	// mechanisms and use of algorithms like de-jittering makes the experience
	// better even under harsh conditions.
	if score > qualityTransitionScore[livekit.ConnectionQuality_GOOD] {
		return livekit.ConnectionQuality_EXCELLENT
	}

	if score > qualityTransitionScore[livekit.ConnectionQuality_POOR] {
		return livekit.ConnectionQuality_GOOD
	}

	if score > qualityTransitionScore[livekit.ConnectionQuality_LOST] {
		return livekit.ConnectionQuality_POOR
	}

	return livekit.ConnectionQuality_LOST
}

// ------------------------------------------

func scoreToMOS(score float64) float32 {
	if score <= 0.0 {
		return 1.0
	}

	if score >= 100.0 {
		return 4.5
	}

	return float32(1.0 + 0.035*score + (0.000007 * score * (score - 60.0) * (100.0 - score)))
}

// ------------------------------------------
