package connectionquality

import (
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
)

const (
	MaxMOS = float32(4.5)

	maxScore  = float64(100.0)
	poorScore = float64(50.0)

	waitForQualityPoor = 25 * time.Second
	waitForQualityGood = 15 * time.Second

	unmuteTimeThreshold = float64(0.5)
)

// ------------------------------------------

type windowStat struct {
	startedAt       time.Time
	duration        time.Duration
	packetsExpected uint32
	packetsLost     uint32
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

	lossEffect := float64(0.0)
	if w.packetsExpected > 0 {
		lossEffect = float64(w.packetsLost) * 100.0 / float64(w.packetsExpected)
	}
	lossEffect *= plw

	return maxScore - delayEffect - lossEffect
}

func (w *windowStat) calculateByteScore(expectedBitrate int64) float64 {
	if expectedBitrate == 0 {
		// unsupported mode
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

func (w *windowStat) getStartTime() time.Time {
	return w.startedAt
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

type windowScore struct {
	stat  *windowStat
	score float64
}

func newWindowScorePacket(stat *windowStat, plw float64) *windowScore {
	return &windowScore{
		stat:  stat,
		score: stat.calculatePacketScore(plw),
	}
}

func newWindowScoreByte(stat *windowStat, expectedBitrate int64) *windowScore {
	return &windowScore{
		stat:  stat,
		score: stat.calculateByteScore(expectedBitrate),
	}
}

func newWindowScoreWithScore(stat *windowStat, score float64) *windowScore {
	return &windowScore{
		stat:  stat,
		score: score,
	}
}

func (w *windowScore) getScore() float64 {
	return w.score
}

func (w *windowScore) getStartTime() time.Time {
	return w.stat.getStartTime()
}

func (w *windowScore) String() string {
	return fmt.Sprintf("stat: {%+v}, score: %0.2f", w.stat, w.score)
}

// ------------------------------------------

type qualityScorerState int

const (
	qualityScorerStateStable qualityScorerState = iota
	qualityScorerStateRecovering
)

func (q qualityScorerState) String() string {
	switch q {
	case qualityScorerStateStable:
		return "STABLE"
	case qualityScorerStateRecovering:
		return "RECOVERING"
	default:
		return fmt.Sprintf("%d", int(q))
	}
}

// ------------------------------------------

type layerTransition struct {
	startedAt time.Time
	bitrate   int64
}

type qualityScorerParams struct {
	PacketLossWeight float64
	Logger           logger.Logger
}

type qualityScorer struct {
	params qualityScorerParams

	lock         sync.RWMutex
	lastUpdateAt time.Time

	score   float64
	state   qualityScorerState
	windows []*windowScore

	mutedAt   time.Time
	unmutedAt time.Time

	maxPPS float64

	transitions []layerTransition
}

func newQualityScorer(params qualityScorerParams) *qualityScorer {
	return &qualityScorer{
		params: params,
		score:  maxScore,
		state:  qualityScorerStateStable,
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

		// stable when muted
		q.state = qualityScorerStateStable
		q.windows = q.windows[:0]
		q.score = maxScore
	} else {
		q.unmutedAt = at
	}
}

func (q *qualityScorer) AddTransition(bitrate int64, at time.Time) {
	q.lock.Lock()
	defer q.lock.Unlock()

	q.transitions = append(q.transitions, layerTransition{
		startedAt: at,
		bitrate:   bitrate,
	})
}

func (q *qualityScorer) Update(stat *windowStat, at time.Time) {
	q.lock.Lock()
	defer q.lock.Unlock()

	// nothing to do when muted or not unmuted for long enough
	// NOTE: it is possible that unmute -> mute -> unmute transition happens in the
	//       same analysis window. On a transition to mute, state immediately moves
	//       to stable and quality EXCELLENT for responsiveness. On an unmute, the
	//       entire window data is considered (as long as enough time has passed since
	//       unmute) including the data before mute.
	if q.isMuted() || !q.isUnmutedEnough(at) {
		q.lastUpdateAt = at
		return
	}

	reason := "none"
	var ws *windowScore
	if stat == nil {
		reason = "dry"
		ws = newWindowScoreWithScore(&windowStat{
			startedAt: q.lastUpdateAt,
			duration:  at.Sub(q.lastUpdateAt),
		}, poorScore)
	} else {
		if stat.packetsExpected == 0 {
			reason = "dry"
			ws = newWindowScoreWithScore(stat, poorScore)
		} else {
			wsPacket := newWindowScorePacket(stat, q.getPacketLossWeight(stat))
			wsByte := newWindowScoreByte(stat, q.getExpectedBitsAndUpdateTransitions(at))
			if wsPacket.getScore() < wsByte.getScore() {
				reason = "packet"
				ws = wsPacket
			} else {
				reason = "bitrate"
				ws = wsByte
			}
		}
	}
	score := ws.getScore()
	cq := scoreToConnectionQuality(score)

	q.lastUpdateAt = at

	// transition to start of recovering on any quality drop
	// WARNING NOTE: comparing protobuf enum values directly (livekit.ConnectionQuality)
	if scoreToConnectionQuality(q.score) > cq {
		q.windows = []*windowScore{ws}
		q.state = qualityScorerStateRecovering
		q.score = score
		q.params.Logger.Infow(
			"quality drop",
			"reaason", reason,
			"score", q.score,
			"quality", scoreToConnectionQuality(q.score),
			"window", ws,
		)
		return
	}

	// if stable and quality continues to be EXCELLENT, stay there.
	if q.state == qualityScorerStateStable && cq == livekit.ConnectionQuality_EXCELLENT {
		q.score = score
		return
	}

	// when recovering, look at a longer window
	q.windows = append(q.windows, ws)
	if !q.prune(at) {
		// minimum recovery duration not satisfied, hold at current quality
		return
	}

	// take median of scores in a longer window to prevent quality reporting oscillations
	sort.Slice(q.windows, func(i, j int) bool { return q.windows[i].getScore() < q.windows[j].getScore() })
	mid := (len(q.windows)+1)/2 - 1
	q.score = q.windows[mid].getScore()
	if scoreToConnectionQuality(q.score) == livekit.ConnectionQuality_EXCELLENT {
		q.state = qualityScorerStateStable
		q.windows = q.windows[:0]
	}
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

	sinceLastUpdate := at.Sub(q.lastUpdateAt)

	return sinceUnmute.Seconds()/sinceLastUpdate.Seconds() > unmuteTimeThreshold
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

func (q *qualityScorer) prune(at time.Time) bool {
	cq := scoreToConnectionQuality(q.score)

	var wait time.Duration
	if cq == livekit.ConnectionQuality_POOR {
		wait = waitForQualityPoor
	} else {
		wait = waitForQualityGood
	}

	startThreshold := at.Add(-wait)
	sort.Slice(q.windows, func(i, j int) bool { return q.windows[i].getStartTime().Before(q.windows[j].getStartTime()) })
	for idx := 0; idx < len(q.windows); idx++ {
		w := q.windows[idx]
		if w.getStartTime().Before(startThreshold) {
			continue
		}

		q.windows = q.windows[idx:]
		break
	}

	// find the oldest window of given quality and check if enough wait happened
	for idx := 0; idx < len(q.windows); idx++ {
		if cq == scoreToConnectionQuality(q.windows[idx].getScore()) {
			return at.Sub(q.windows[idx].getStartTime()) >= wait
		}
	}

	return false
}

func (q *qualityScorer) getExpectedBitsAndUpdateTransitions(at time.Time) int64 {
	if len(q.transitions) == 0 {
		return 0
	}

	var startedAt time.Time
	totalBits := float64(0.0)
	for idx := 0; idx < len(q.transitions)-1; idx++ {
		lt := &q.transitions[idx]
		ltNext := &q.transitions[idx+1]

		if lt.startedAt.After(q.lastUpdateAt) {
			startedAt = lt.startedAt
		} else {
			startedAt = q.lastUpdateAt
		}
		totalBits += ltNext.startedAt.Sub(startedAt).Seconds() * float64(lt.bitrate)
	}
	// last transition
	lt := &q.transitions[len(q.transitions)-1]
	if lt.startedAt.After(q.lastUpdateAt) {
		startedAt = lt.startedAt
	} else {
		startedAt = q.lastUpdateAt
	}
	totalBits += at.Sub(startedAt).Seconds() * float64(lt.bitrate)

	// set up last bit rate as the startig bit rate for next analysis window
	q.transitions = []layerTransition{layerTransition{
		startedAt: at,
		bitrate:   lt.bitrate,
	}}

	return int64(totalBits)
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
