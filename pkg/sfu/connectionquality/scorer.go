package connectionquality

import (
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils"
)

const (
	MaxMOS = float32(4.5)
	MinMOS = float32(1.0)

	maxScore  = float64(100.0)
	poorScore = float64(30.0)
	minScore  = float64(20.0)

	increaseFactor = float64(0.4) // slower increase, i. e. when score is recovering move up slower -> conservative
	decreaseFactor = float64(0.7) // faster decrease, i. e. when score is dropping move down faster -> aggressive to be responsive to quality drops

	distanceWeight = float64(35.0) // each spatial layer missed drops a quality level

	unmuteTimeThreshold = float64(0.5)
)

// ------------------------------------------

type windowStat struct {
	startedAt         time.Time
	duration          time.Duration
	packetsExpected   uint32
	packetsLost       uint32
	packetsMissing    uint32
	packetsOutOfOrder uint32
	bytes             uint64
	rttMax            uint32
	jitterMax         float64
}

func (w *windowStat) calculatePacketScore(plw float64, includeRTT bool, includeJitter bool) float64 {
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
	// 3. down stream client reports a certain number of loss
	// 4. while processing that, up stream could have retransmitted missing packets
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
	if w.packetsExpected > 0 {
		lossEffect = float64(actualLost) * 100.0 / float64(w.packetsExpected)
	}
	lossEffect *= plw

	score := maxScore - delayEffect - lossEffect
	if score < 0.0 {
		score = 0.0
	}

	return score
}

func (w *windowStat) calculateBitrateScore(expectedBitrate int64) float64 {
	if expectedBitrate == 0 {
		// unsupported mode OR all layers stopped
		return maxScore
	}

	var score float64
	if w.bytes != 0 {
		// using the ratio of expectedBitrate / actualBitrate
		// the quality inflection points are approximately
		// GOOD at ~2.7x, POOR at ~20.1x
		score = maxScore - 20*math.Log(float64(expectedBitrate)/float64(w.bytes*8))
		if score > maxScore {
			score = maxScore
		}
		if score < 0.0 {
			score = 0.0
		}
	}

	return score
}

func (w *windowStat) String() string {
	return fmt.Sprintf("start: %+v, dur: %+v, pe: %d, pl: %d, pm: %d, pooo: %d, b: %d, rtt: %d, jitter: %0.2f",
		w.startedAt,
		w.duration,
		w.packetsExpected,
		w.packetsLost,
		w.packetsMissing,
		w.packetsOutOfOrder,
		w.bytes,
		w.rttMax,
		w.jitterMax,
	)
}

// ------------------------------------------

type qualityScorerParams struct {
	PacketLossWeight float64
	IncludeRTT       bool
	IncludeJitter    bool
	Logger           logger.Logger
}

type qualityScorer struct {
	params qualityScorerParams

	lock         sync.RWMutex
	lastUpdateAt time.Time

	score float64
	stat  windowStat

	mutedAt   time.Time
	unmutedAt time.Time

	layerMutedAt   time.Time
	layerUnmutedAt time.Time

	pausedAt  time.Time
	resumedAt time.Time

	maxPPS float64

	aggregateBitrate *utils.TimedAggregator[int64]
	layerDistance    *utils.TimedAggregator[float64]
}

func newQualityScorer(params qualityScorerParams) *qualityScorer {
	return &qualityScorer{
		params: params,
		score:  maxScore,
		aggregateBitrate: utils.NewTimedAggregator[int64](utils.TimedAggregatorParams{
			CapNegativeValues: true,
		}),
		layerDistance: utils.NewTimedAggregator[float64](utils.TimedAggregatorParams{
			CapNegativeValues: true,
		}),
	}
}

func (q *qualityScorer) startAtLocked(at time.Time) {
	q.lastUpdateAt = at
}

func (q *qualityScorer) StartAt(at time.Time) {
	q.lock.Lock()
	defer q.lock.Unlock()

	q.startAtLocked(at)
}

func (q *qualityScorer) Start() {
	q.lock.Lock()
	defer q.lock.Unlock()

	q.startAtLocked(time.Now())
}

func (q *qualityScorer) updateMuteAtLocked(isMuted bool, at time.Time) {
	if isMuted {
		q.mutedAt = at
		q.score = maxScore
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
			q.score = maxScore
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
			q.score = poorScore
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
	expectedBitrate, _, err := q.aggregateBitrate.GetAggregateAndRestartAt(at)
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
	//       set to poorScore for responsiveness. The layer transision is reest.
	//       On a resume, quality climbs back up using normal operation.
	if q.isMuted() || !q.isUnmutedEnough(at) || q.isLayerMuted() || q.isPaused() {
		q.lastUpdateAt = at
		return
	}

	plw := q.getPacketLossWeight(stat)
	reason := "none"
	var score float64
	if stat.packetsExpected == 0 {
		reason = "dry"
		score = poorScore
	} else {
		packetScore := stat.calculatePacketScore(plw, q.params.IncludeRTT, q.params.IncludeJitter)
		bitrateScore := stat.calculateBitrateScore(expectedBitrate)
		layerScore := math.Max(math.Min(maxScore, maxScore-(expectedDistance*distanceWeight)), 0.0)

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

		factor := increaseFactor
		if score < q.score {
			factor = decreaseFactor
		}
		score = factor*score + (1.0-factor)*q.score
	}
	if score < minScore {
		// lower bound to prevent score from becoming very small values due to extreme conditions.
		// Without a lower bound, it can get so low that it takes a long time to climb back to
		// better quality even under excellent conditions.
		score = minScore
	}
	// WARNING NOTE: comparing protobuf enum values directly (livekit.ConnectionQuality)
	if scoreToConnectionQuality(q.score) > scoreToConnectionQuality(score) {
		q.params.Logger.Infow(
			"quality drop",
			"reason", reason,
			"prevScore", q.score,
			"prevQuality", scoreToConnectionQuality(q.score),
			"prevStat", &q.stat,
			"score", score,
			"quality", scoreToConnectionQuality(score),
			"stat", stat,
			"packetLossWeight", plw,
			"maxPPS", q.maxPPS,
			"expectedBitrate", expectedBitrate,
			"expectedDistance", expectedDistance,
		)
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

	return validDuration.Seconds()/sinceLastUpdate.Seconds() > unmuteTimeThreshold
}

func (q *qualityScorer) isLayerMuted() bool {
	return !q.layerMutedAt.IsZero() && (q.layerUnmutedAt.IsZero() || q.layerMutedAt.After(q.layerUnmutedAt))
}

func (q *qualityScorer) isPaused() bool {
	return !q.pausedAt.IsZero() && (q.resumedAt.IsZero() || q.pausedAt.After(q.resumedAt))
}

func (q *qualityScorer) getPacketLossWeight(stat *windowStat) float64 {
	if stat == nil || stat.duration == 0 {
		return q.params.PacketLossWeight
	}

	// packet loss is weighted by comparing against max packet rate seen.
	// this is to handle situations like DTX in audio and variable bit rate tracks like screen share.
	// and the effect of loss is not pronounced in those scenarios (audio silence, statis screen share).
	// for example, DTX typically uses only 5% of packets of full packet rate. at that rate,
	// packet loss weight is reduced to ~22% of configured weight (i. e. sqrt(0.05) * configured weight)
	pps := float64(stat.packetsExpected) / stat.duration.Seconds()
	if pps > q.maxPPS {
		q.maxPPS = pps
		q.params.Logger.Debugw("updating maxPPS", "expected", stat.packetsExpected, "duration", stat.duration.Seconds(), "pps", pps)
	}

	if q.maxPPS == 0 {
		return q.params.PacketLossWeight
	}

	packetRatio := pps / q.maxPPS
	return packetRatio * packetRatio * q.params.PacketLossWeight
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
	if score > 80.0 {
		return livekit.ConnectionQuality_EXCELLENT
	}

	if score > 40.0 {
		return livekit.ConnectionQuality_GOOD
	}

	return livekit.ConnectionQuality_POOR
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
