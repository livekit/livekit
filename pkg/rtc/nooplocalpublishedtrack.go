package rtc

type NoOpLocalPublishedTrack struct {
}

func (t *NoOpLocalPublishedTrack) GetAudioLevel() (level uint8, active bool) {
	return SilentAudioLevel, false
}

func (t *NoOpLocalPublishedTrack) GetConnectionScore() float64 {
	return 0
}

func (t *NoOpLocalPublishedTrack) SdpCid() (sdpCid string) {
	return
}

func (t *NoOpLocalPublishedTrack) SignalCid() (signalCid string) {
	return
}
