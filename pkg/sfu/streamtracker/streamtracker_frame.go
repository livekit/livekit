package streamtracker

import (
	"math"
	"time"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/protocol/logger"
)

const (
	checkInterval       = 500 * time.Millisecond
	staleWindowFactor   = 5
	frameRateResolution = float64(0.01) // 1 frame every 100 seconds
)

type StreamTrackerFrameParams struct {
	Config    config.StreamTrackerFrameConfig
	ClockRate uint32
	Logger    logger.Logger
}

type StreamTrackerFrame struct {
	params StreamTrackerFrameParams

	initialized bool

	tsInitialized bool
	oldestTS      uint32
	newestTS      uint32
	numFrames     int

	lowestFrameRate   float64
	evalInterval      time.Duration
	lastStatusCheckAt time.Time
}

func NewStreamTrackerFrame(params StreamTrackerFrameParams) StreamTrackerImpl {
	s := &StreamTrackerFrame{
		params: params,
	}
	s.Reset()
	return s
}

func (s *StreamTrackerFrame) Start() {
}

func (s *StreamTrackerFrame) Stop() {
}

func (s *StreamTrackerFrame) Reset() {
	s.initialized = false

	s.tsInitialized = false
	s.oldestTS = 0
	s.newestTS = 0
	s.numFrames = 0

	s.lowestFrameRate = 0.0
	s.updateEvalInterval()
	s.lastStatusCheckAt = time.Time{}
}

func (s *StreamTrackerFrame) GetCheckInterval() time.Duration {
	return checkInterval
}

func (s *StreamTrackerFrame) Observe(hasMarker bool, ts uint32) StreamStatusChange {
	if !s.initialized {
		s.initialized = true
		if hasMarker {
			s.tsInitialized = true
			s.oldestTS = ts
			s.newestTS = ts
			s.numFrames = 1
		}
		return StreamStatusChangeActive
	}

	if hasMarker {
		if !s.tsInitialized {
			s.tsInitialized = true
			s.oldestTS = ts
			s.newestTS = ts
			s.numFrames = 1
		} else {
			diff := ts - s.oldestTS
			if diff > (1 << 31) {
				s.oldestTS = ts
			}
			diff = ts - s.newestTS
			if diff < (1 << 31) {
				s.newestTS = ts
			}
			s.numFrames++
		}
	}
	return StreamStatusChangeNone
}

func (s *StreamTrackerFrame) CheckStatus() StreamStatusChange {
	if !s.initialized {
		// should not be getting called when not initialized, but be safe
		return StreamStatusChangeNone
	}

	// calculate frame rate since last check
	frameRate := float64(0.0)
	diff := s.newestTS - s.oldestTS
	if diff > 0 || s.numFrames > 1 {
		if diff > s.params.ClockRate*staleWindowFactor {
			s.params.Logger.Infow("eval window might be stale", "numFrames", s.numFrames, "timeElapsed", float64(diff)/float64(s.params.ClockRate))
			// STREAM-TRACKER-FRAME-TODO: might need to protect against one frame, long pause and then one or more frames, i. e. window getting stale.
			// One possible option is to reset the fps measurement variables (tsInitialized, oldestTS, newestTS, numFrames, lowestFrameRate, evelInterval)
			// and restart the lowest frame rate calulation process.
		}
		frameRate = float64(s.params.ClockRate) / float64(diff) * float64(s.numFrames-1)
		frameRate = math.Round(frameRate/frameRateResolution) * frameRateResolution
	}

	if s.lowestFrameRate == 0.0 {
		if frameRate == 0.0 {
			// need at least two frames to kick things off
			return StreamStatusChangeNone
		}

		s.lowestFrameRate = frameRate
		s.updateEvalInterval()
		s.params.Logger.Infow("initializing lowest frame rate", "lowestFPS", s.lowestFrameRate, "evalInterval", s.evalInterval)
	} else {
		// check only at intervals based on lowest seen frame rate
		if s.lastStatusCheckAt.IsZero() {
			s.lastStatusCheckAt = time.Now()
		}
		if time.Since(s.lastStatusCheckAt) < s.evalInterval {
			return StreamStatusChangeNone
		}
		s.lastStatusCheckAt = time.Now()
	}

	// reset for next evaluation interval
	s.oldestTS = s.newestTS
	s.numFrames = 1

	// STREAM-TRACKER-FRAME-TODO: this will run into challenges for frame rate falling steeply, how to address that
	// look at some referential rules (between layers) for possibilities to solve it. Currently, this is addressed
	// by setting a source aware min FPS to ensure evaluation window in long enough
	// update lowest seen frame rate
	if frameRate > 0.0 && s.lowestFrameRate > frameRate {
		s.lowestFrameRate = frameRate
		s.updateEvalInterval()
		s.params.Logger.Infow("updating lowest frame rate", "lowestFPS", s.lowestFrameRate, "evalInterval", s.evalInterval)
	}

	if frameRate == 0.0 {
		return StreamStatusChangeStopped
	}

	return StreamStatusChangeActive
}

func (s *StreamTrackerFrame) updateEvalInterval() {
	s.evalInterval = checkInterval
	if s.lowestFrameRate > 0 {
		lowestFrameRateInterval := time.Duration(float64(time.Second) / s.lowestFrameRate)
		if lowestFrameRateInterval > s.evalInterval {
			s.evalInterval = lowestFrameRateInterval
		}
	}
	if s.params.Config.MinFPS > 0 {
		minFPSInterval := time.Duration(float64(time.Second) / s.params.Config.MinFPS)
		if minFPSInterval > s.evalInterval {
			s.evalInterval = minFPSInterval
		}
	}
}
