package connectionquality

import (
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
)

const (
	MaxMOS = float32(4.5)

	maxScore  = float64(100.0)
	poorScore = float64(50.0)

	increaseFactor = float64(0.4) // slow increase
	decreaseFactor = float64(0.8) // fast decrease

	distanceWeight = float64(25.0) // each spatial layer missed drops a quality level

	unmuteTimeThreshold = float64(0.5)
)

// ------------------------------------------

type windowStat struct {
	startedAt       time.Time
	duration        time.Duration
	packetsExpected uint32
	packetsLost     uint32
	packetsMissing  uint32
	bytes           uint64
	rttMax          uint32
	jitterMax       float64
}

func (w *windowStat) calculatePacketScore(plw float64) float64 {
	// this is based on simplified E-model based on packet loss, rtt, jitter as
	// outlined at https://www.pingman.com/kb/article/how-is-mos-calculated-in-pingplotter-pro-50.html.
	effectiveDelay := (float64(w.rttMax) / 2.0) + ((w.jitterMax * 2.0) / 1000.0)
	delayEffect := effectiveDelay / 40.0
	if effectiveDelay > 160.0 {
		delayEffect = (effectiveDelay - 120.0) / 10.0
	}

	actualLost := w.packetsLost - w.packetsMissing
	if int32(actualLost) < 0 {
		actualLost = 0
	}

	lossEffect := float64(0.0)
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

	score := float64(0.0)
	if w.bytes != 0 {
		// using the ratio of expectedBitrate / actualBitrate
		// the quality inflection points are approximately
		// GOOD at ~2.7x, POOR at ~7.5x
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
	return fmt.Sprintf("start: %+v, dur: %+v, pe: %d, pl: %d, b: %d, rtt: %d, jitter: %0.2f",
		w.startedAt,
		w.duration,
		w.packetsExpected,
		w.packetsLost,
		w.bytes,
		w.rttMax,
		w.jitterMax,
	)
}

// ------------------------------------------

type bitrateTransition struct {
	startedAt time.Time
	bitrate   int64
}

type layerTransition struct {
	startedAt time.Time
	distance  float64
}

type qualityScorerParams struct {
	PacketLossWeight float64
	Logger           logger.Logger
}

type qualityScorer struct {
	params qualityScorerParams

	lock         sync.RWMutex
	lastUpdateAt time.Time

	score float64

	mutedAt   time.Time
	unmutedAt time.Time

	bitrateMutedAt   time.Time
	bitrateUnmutedAt time.Time

	maxPPS float64

	bitrateTransitions []bitrateTransition
	layerTransitions   []layerTransition
}

func newQualityScorer(params qualityScorerParams) *qualityScorer {
	return &qualityScorer{
		params: params,
		score:  maxScore,
	}
}

func (q *qualityScorer) Start(at time.Time) {
	q.lock.Lock()
	defer q.lock.Unlock()

	q.lastUpdateAt = at
}

func (q *qualityScorer) UpdateMute(isMuted bool, at time.Time) {
	q.lock.Lock()
	defer q.lock.Unlock()

	if isMuted {
		q.mutedAt = at
		q.score = maxScore
	} else {
		q.unmutedAt = at
	}
}

func (q *qualityScorer) AddBitrateTransition(bitrate int64, at time.Time) {
	q.lock.Lock()
	defer q.lock.Unlock()

	q.bitrateTransitions = append(q.bitrateTransitions, bitrateTransition{
		startedAt: at,
		bitrate:   bitrate,
	})

	if bitrate == 0 {
		q.bitrateMutedAt = at
		q.score = maxScore
	} else {
		if q.bitrateUnmutedAt.IsZero() || q.bitrateMutedAt.After(q.bitrateUnmutedAt) {
			q.bitrateUnmutedAt = at
		}
	}
}

func (q *qualityScorer) AddLayerTransition(distance float64, at time.Time) {
	q.lock.Lock()
	defer q.lock.Unlock()

	q.layerTransitions = append(q.layerTransitions, layerTransition{
		startedAt: at,
		distance:  distance,
	})
}

func (q *qualityScorer) Update(stat *windowStat, at time.Time) {
	q.lock.Lock()
	defer q.lock.Unlock()

	// always update transitions
	expectedBitrate := q.getExpectedBitsAndUpdateTransitions(at)
	expectedDistance := q.getExpectedDistanceAndUpdateTransitions(at)

	// nothing to do when muted or not unmuted for long enough
	// NOTE: it is possible that unmute -> mute -> unmute transition happens in the
	//       same analysis window. On a transition to mute, state immediately moves
	//       to stable and quality EXCELLENT for responsiveness. On an unmute, the
	//       entire window data is considered (as long as enough time has passed since
	//       unmute) including the data before mute.
	if q.isMuted() || !q.isUnmutedEnough(at) || q.isBitrateMuted() {
		q.lastUpdateAt = at
		return
	}

	reason := "none"
	var score float64
	if stat.packetsExpected == 0 {
		reason = "dry"
		score = poorScore
	} else {
		packetScore := stat.calculatePacketScore(q.getPacketLossWeight(stat))
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
	}
	factor := increaseFactor
	if score < q.score {
		factor = decreaseFactor
	}
	score = factor*score + (1.0-factor)*q.score
	// WARNING NOTE: comparing protobuf enum values directly (livekit.ConnectionQuality)
	if scoreToConnectionQuality(q.score) > scoreToConnectionQuality(score) {
		q.params.Logger.Infow(
			"quality drop",
			"reason", reason,
			"prevScore", q.score,
			"prevQuality", scoreToConnectionQuality(q.score),
			"score", score,
			"quality", scoreToConnectionQuality(score),
			"stat", stat,
			"expectedBitrate", expectedBitrate,
			"expectedDistance", expectedDistance,
		)
	}

	q.score = score
	q.lastUpdateAt = at
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

	var sinceLayersUnmute time.Duration
	if q.bitrateUnmutedAt.IsZero() {
		sinceLayersUnmute = at.Sub(q.lastUpdateAt)
	} else {
		sinceLayersUnmute = at.Sub(q.bitrateUnmutedAt)
	}

	validDuration := sinceUnmute
	if sinceLayersUnmute < validDuration {
		validDuration = sinceLayersUnmute
	}

	sinceLastUpdate := at.Sub(q.lastUpdateAt)

	return validDuration.Seconds()/sinceLastUpdate.Seconds() > unmuteTimeThreshold
}

func (q *qualityScorer) isBitrateMuted() bool {
	return !q.bitrateMutedAt.IsZero() && (q.bitrateUnmutedAt.IsZero() || q.bitrateMutedAt.After(q.bitrateUnmutedAt))
}

func (q *qualityScorer) getPacketLossWeight(stat *windowStat) float64 {
	if stat == nil {
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
	}

	return math.Sqrt(pps/q.maxPPS) * q.params.PacketLossWeight
}

func (q *qualityScorer) getExpectedBitsAndUpdateTransitions(at time.Time) int64 {
	if len(q.bitrateTransitions) == 0 {
		return 0
	}

	var startedAt time.Time
	totalBits := float64(0.0)
	for idx := 0; idx < len(q.bitrateTransitions)-1; idx++ {
		bt := &q.bitrateTransitions[idx]
		btNext := &q.bitrateTransitions[idx+1]

		if bt.startedAt.After(q.lastUpdateAt) {
			startedAt = bt.startedAt
		} else {
			startedAt = q.lastUpdateAt
		}
		totalBits += btNext.startedAt.Sub(startedAt).Seconds() * float64(bt.bitrate)
	}

	// last transition
	bt := &q.bitrateTransitions[len(q.bitrateTransitions)-1]
	if bt.startedAt.After(q.lastUpdateAt) {
		startedAt = bt.startedAt
	} else {
		startedAt = q.lastUpdateAt
	}
	totalBits += at.Sub(startedAt).Seconds() * float64(bt.bitrate)

	// set up last bit rate as the startig bit rate for next analysis window
	q.bitrateTransitions = []bitrateTransition{bitrateTransition{
		startedAt: at,
		bitrate:   bt.bitrate,
	}}

	return int64(totalBits)
}

func (q *qualityScorer) getExpectedDistanceAndUpdateTransitions(at time.Time) float64 {
	if len(q.layerTransitions) == 0 {
		return 0
	}

	var startedAt time.Time
	totalDistance := float64(0.0)
	totalDuration := time.Duration(0)
	for idx := 0; idx < len(q.layerTransitions)-1; idx++ {
		lt := &q.layerTransitions[idx]
		ltNext := &q.layerTransitions[idx+1]

		if lt.startedAt.After(q.lastUpdateAt) {
			startedAt = lt.startedAt
		} else {
			startedAt = q.lastUpdateAt
		}
		dur := ltNext.startedAt.Sub(startedAt)
		totalDuration += dur

		dist := lt.distance
		if dist < 0.0 {
			// negative distances are overshoot, that does not compensate for shortfalls, so use optimal, i. e. 0 distance when overshooting
			dist = 0.0
		}
		totalDistance += dur.Seconds() * float64(dist)
	}

	// last transition
	lt := &q.layerTransitions[len(q.layerTransitions)-1]
	if lt.startedAt.After(q.lastUpdateAt) {
		startedAt = lt.startedAt
	} else {
		startedAt = q.lastUpdateAt
	}
	dur := at.Sub(startedAt)
	totalDuration += dur

	dist := lt.distance
	if dist < 0.0 {
		dist = 0.0
	}
	totalDistance += dur.Seconds() * float64(dist)

	// set up last distance as the startig distance for next analysis window
	q.layerTransitions = []layerTransition{layerTransition{
		startedAt: at,
		distance:  lt.distance,
	}}

	if totalDuration == 0 {
		return 0
	}

	return totalDistance / totalDuration.Seconds()
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
	// R-factor -> livekit.ConnectionQuality scale mapping based on
	// https://www.itu.int/ITU-T/2005-2008/com12/emodelv1/tut.htm
	if score > 80.0 {
		return livekit.ConnectionQuality_EXCELLENT
	}

	if score > 60.0 {
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
