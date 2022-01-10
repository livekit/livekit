package rtc

type NoOpLocalMediaTrack struct {
}

func (t *NoOpLocalMediaTrack) NotifySubscriberNodeMediaLoss(_nodeID string, _fractionalLoss uint8) {
}
