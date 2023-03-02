// RAJA-TODO
// maintain max_pps and do some adjustment for pps reduction
// maintain max fps and do some adjustment for fps reduction
package connectionquality

import (
	"fmt"
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

	unmuteTimeThreshold = float64(0.75)
)

// ------------------------------------------

type windowStat struct {
	startedAt       time.Time
	duration        time.Duration
	packetsExpected uint32
	packetsLost     uint32
	rttMax          uint32
	jitterMax       float64
}

func (w *windowStat) calculateScore(plw float64) float64 {
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

func (w *windowStat) getStartTime() time.Time {
	return w.startedAt
}

func (w *windowStat) String() string {
	return fmt.Sprintf("start: %+v, dur: %+v, pe: %d, pl: %d, rtt: %d, jitter: %0.2f",
		w.startedAt,
		w.duration,
		w.packetsExpected,
		w.packetsLost,
		w.rttMax,
		w.jitterMax,
	)
}

// ------------------------------------------

type windowScore struct {
	stat  *windowStat
	score float64
}

func newWindowScore(stat *windowStat, plw float64) *windowScore {
	return &windowScore{
		stat:  stat,
		score: stat.calculateScore(plw),
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

type qualityScoreState int

const (
	qualityScoreStateStable qualityScoreState = iota
	qualityScoreStateRecovering
)

func (q qualityScoreState) String() string {
	switch q {
	case qualityScoreStateStable:
		return "STABLE"
	case qualityScoreStateRecovering:
		return "RECOVERING"
	default:
		return fmt.Sprintf("%d", int(q))
	}
}

// ------------------------------------------

type qualityScoreParams struct {
	PacketLossWeight float64
	Logger           logger.Logger
}

type qualityScore struct {
	params qualityScoreParams

	lock         sync.RWMutex
	lastUpdateAt time.Time

	score   float64
	state   qualityScoreState
	windows []*windowScore

	mutedAt   time.Time
	unmutedAt time.Time
}

func newQualityScore(params qualityScoreParams) *qualityScore {
	return &qualityScore{
		params: params,
		score:  maxScore,
		state:  qualityScoreStateStable,
	}
}

func (q *qualityScore) Start() {
	q.lock.Lock()
	defer q.lock.Unlock()

	q.lastUpdateAt = time.Now()
}

func (q *qualityScore) UpdateMute(isMuted bool) {
	q.lock.Lock()
	defer q.lock.Unlock()

	if isMuted {
		q.mutedAt = time.Now()
	} else {
		q.unmutedAt = time.Now()
	}
}

func (q *qualityScore) Update(stat *windowStat) {
	q.lock.Lock()
	defer q.lock.Unlock()

	if stat == nil {
		// if no stat, i. e. no packets received, could be due to mute or stream has stopped due to some constraint
		q.updateNoPacketsReceived()
	} else {
		q.updatePacketsReceived(stat)
	}

	q.lastUpdateAt = time.Now()
}

func (q *qualityScore) updatePacketsReceived(stat *windowStat) {
	ws := newWindowScore(stat, q.params.PacketLossWeight)
	q.params.Logger.Infow("quality stat", "stat", stat, "window", ws) // REMOVE
	score := ws.getScore()
	cq := scoreToConnectionQuality(score)

	// transition to start of recovering on any quality drop
	// WARNING NOTE: comparing protobuf enum values directly (livekit.ConnectionQuality)
	if scoreToConnectionQuality(q.score) > cq {
		q.windows = []*windowScore{ws}
		q.state = qualityScoreStateRecovering
		q.score = score
		return
	}

	// if stable and quality continues to be EXCELLENT, stay there.
	if q.state == qualityScoreStateStable && cq == livekit.ConnectionQuality_EXCELLENT {
		q.score = score
		return
	}

	// when recovering, look at a longer window
	q.windows = append(q.windows, ws)
	q.prune()

	recoveringDuration := time.Since(q.windows[0].getStartTime())
	cq = scoreToConnectionQuality(q.score)
	if (cq == livekit.ConnectionQuality_GOOD && recoveringDuration < waitForQualityGood) ||
		(cq == livekit.ConnectionQuality_POOR && recoveringDuration < waitForQualityPoor) {
		// hold at current quality till there is enough data while recovering
		return
	}

	// take median of scores in a longer window to prevent quality reporting oscillations
	sort.Slice(q.windows, func(i, j int) bool { return q.windows[i].getScore() < q.windows[j].getScore() })
	mid := (len(q.windows)+1)/2 - 1
	q.score = q.windows[mid].getScore()
	if scoreToConnectionQuality(q.score) == livekit.ConnectionQuality_EXCELLENT {
		q.state = qualityScoreStateStable
		q.windows = q.windows[:0]
	}
}

func (q *qualityScore) updateNoPacketsReceived() {
	isMuted := !q.mutedAt.IsZero() && (!q.unmutedAt.IsZero() || q.mutedAt.After(q.unmutedAt))
	if isMuted {
		// stable when muted
		q.state = qualityScoreStateStable
		q.windows = q.windows[:0]
		q.score = maxScore
		return
	}

	// when not muted, if has been unmuted for enough time in the analysis window, transition to POOR quality
	var sinceUnmute time.Duration
	if q.unmutedAt.IsZero() {
		sinceUnmute = time.Since(q.lastUpdateAt)
	} else {
		sinceUnmute = time.Since(q.unmutedAt)
	}
	sinceLastUpdate := time.Since(q.lastUpdateAt)
	if sinceUnmute.Seconds()/sinceLastUpdate.Seconds() < unmuteTimeThreshold {
		return
	}

	ws := newWindowScoreWithScore(&windowStat{
		startedAt: q.lastUpdateAt,
		duration:  sinceLastUpdate,
	}, poorScore)
	score := ws.getScore()
	// transition to start of recovering on any quality drop
	// WARNING NOTE: comparing protobuf enum values directly (livekit.ConnectionQuality)
	if scoreToConnectionQuality(q.score) > scoreToConnectionQuality(score) {
		q.windows = []*windowScore{ws}
		q.state = qualityScoreStateRecovering
		q.score = score
		return
	}

	q.windows = append(q.windows, ws)
}

func (q *qualityScore) prune() {
	cq := scoreToConnectionQuality(q.score)

	var wait time.Duration
	if cq == livekit.ConnectionQuality_POOR {
		wait = waitForQualityPoor
	} else {
		wait = waitForQualityGood
	}

	startThreshold := time.Now().Add(-2 * wait)
	sort.Slice(q.windows, func(i, j int) bool { return q.windows[i].getStartTime().Before(q.windows[j].getStartTime()) })
	for idx := 0; idx < len(q.windows); idx++ {
		w := q.windows[idx]
		if w.getStartTime().Before(startThreshold) {
			continue
		}

		q.windows = q.windows[idx:]
		break
	}
}

func (q *qualityScore) GetScoreAndQuality() (float32, livekit.ConnectionQuality) {
	q.lock.RLock()
	defer q.lock.RUnlock()

	return float32(q.score), scoreToConnectionQuality(q.score)
}

func (q *qualityScore) GetMOSAndQuality() (float32, livekit.ConnectionQuality) {
	q.lock.RLock()
	defer q.lock.RUnlock()

	return scoreToMOS(q.score), scoreToConnectionQuality(q.score)
}

// ------------------------------------------

func scoreToConnectionQuality(score float64) livekit.ConnectionQuality {
	if score > 80 {
		return livekit.ConnectionQuality_EXCELLENT
	}

	if score > 60 {
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
