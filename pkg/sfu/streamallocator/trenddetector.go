package streamallocator

import (
	"fmt"
	"time"

	"github.com/livekit/protocol/logger"
)

// ------------------------------------------------

type TrendDirection int

const (
	TrendDirectionNeutral TrendDirection = iota
	TrendDirectionUpward
	TrendDirectionDownward
)

func (t TrendDirection) String() string {
	switch t {
	case TrendDirectionNeutral:
		return "NEUTRAL"
	case TrendDirectionUpward:
		return "UPWARD"
	case TrendDirectionDownward:
		return "DOWNWARD"
	default:
		return fmt.Sprintf("%d", int(t))
	}
}

// ------------------------------------------------

type TrendDetectorParams struct {
	Name                   string
	Logger                 logger.Logger
	RequiredSamples        int
	DownwardTrendThreshold float64
	CollapseThreshold      time.Duration
}

type TrendDetector struct {
	params TrendDetectorParams

	startTime    time.Time
	numSamples   int
	values       []int64
	lowestValue  int64
	highestValue int64

	hasFallen    bool
	lastSampleAt time.Time

	direction TrendDirection
}

func NewTrendDetector(params TrendDetectorParams) *TrendDetector {
	return &TrendDetector{
		params:    params,
		startTime: time.Now(),
		direction: TrendDirectionNeutral,
	}
}

func (t *TrendDetector) Seed(value int64) {
	if len(t.values) != 0 {
		return
	}

	t.values = append(t.values, value)
	t.lastSampleAt = time.Now()
	t.hasFallen = false
}

func (t *TrendDetector) AddValue(value int64) {
	t.numSamples++
	if t.lowestValue == 0 || value < t.lowestValue {
		t.lowestValue = value
	}
	if value > t.highestValue {
		t.highestValue = value
	}

	// Ignore duplicate values in collapse window.
	//
	// Bandwidth estimate is received periodically. If the estimate does not change, it will be repeated.
	// When there is congestion, there are several estimates received with decreasing values.
	//
	// Using a sliding window, collapsing repeated values and waiting for falling trend is to ensure that
	// the reaction is not too fast, i. e. reacting to falling values too quick could mean a lot of re-allocation
	// resulting in layer switches, key frames and more congestion.
	//
	// But, on the flip side, estimate could fall once or twice withing a sliding window and stay there.
	// In those cases, using a collapse window to record value even if it is duplicate. By doing that,
	// a trend could be detected eventually. If will be delayed, but that is fine with slow changing estimates.
	lastValue := int64(0)
	if len(t.values) != 0 {
		lastValue = t.values[len(t.values)-1]
	}
	if lastValue == value && t.params.CollapseThreshold > 0 {
		if !t.hasFallen || (!t.lastSampleAt.IsZero() && time.Since(t.lastSampleAt) < t.params.CollapseThreshold) {
			return
		}
	}

	if lastValue > value {
		t.hasFallen = true
	}
	t.lastSampleAt = time.Now()

	if len(t.values) == t.params.RequiredSamples {
		t.values = t.values[1:]
	}
	t.values = append(t.values, value)

	t.updateDirection()
}

func (t *TrendDetector) GetLowest() int64 {
	return t.lowestValue
}

func (t *TrendDetector) GetHighest() int64 {
	return t.highestValue
}

func (t *TrendDetector) GetValues() []int64 {
	return t.values
}

func (t *TrendDetector) GetDirection() TrendDirection {
	return t.direction
}

func (t *TrendDetector) ToString() string {
	now := time.Now()
	elapsed := now.Sub(t.startTime).Seconds()
	return fmt.Sprintf("n: %s, t: %+v|%+v|%.2fs, v: %d|%d|%d|%+v|%.2f",
		t.params.Name,
		t.startTime.Format(time.UnixDate), now.Format(time.UnixDate), elapsed,
		t.numSamples, t.lowestValue, t.highestValue, t.values, kendallsTau(t.values))
}

func (t *TrendDetector) updateDirection() {
	if len(t.values) < t.params.RequiredSamples {
		t.direction = TrendDirectionNeutral
		return
	}

	// using Kendall's Tau to find trend
	kt := kendallsTau(t.values)

	t.direction = TrendDirectionNeutral
	switch {
	case kt > 0:
		t.direction = TrendDirectionUpward
	case kt < t.params.DownwardTrendThreshold:
		t.direction = TrendDirectionDownward
	}
}

// ------------------------------------------------

func kendallsTau(values []int64) float64 {
	concordantPairs := 0
	discordantPairs := 0

	for i := 0; i < len(values)-1; i++ {
		for j := i + 1; j < len(values); j++ {
			if values[i] < values[j] {
				concordantPairs++
			} else if values[i] > values[j] {
				discordantPairs++
			}
		}
	}

	if (concordantPairs + discordantPairs) == 0 {
		return 0.0
	}

	return (float64(concordantPairs) - float64(discordantPairs)) / (float64(concordantPairs) + float64(discordantPairs))
}
