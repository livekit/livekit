package sfu

import "time"

const (
	quarterResolution = "q"
	halfResolution    = "h"
	fullResolution    = "f"
)

type SimulcastConfig struct {
	BestQualityFirst    bool `mapstructure:"bestqualityfirst"`
	EnableTemporalLayer bool `mapstructure:"enabletemporallayer"`
}

type simulcastTrackHelpers struct {
	switchDelay       time.Time
	temporalSupported bool
	temporalEnabled   bool
	lTSCalc           atomicInt64

	// VP8Helper temporal helpers
	pRefPicID  atomicUint16
	refPicID   atomicUint16
	lPicID     atomicUint16
	pRefTlZIdx atomicUint8
	refTlZIdx  atomicUint8
	lTlZIdx    atomicUint8
}
