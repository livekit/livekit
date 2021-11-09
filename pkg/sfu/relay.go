package sfu

func RelayWithFanOutDataChannels() func(r *relayPeer) {
	return func(r *relayPeer) {
		r.relayFanOutDataChannels = true
	}
}

func RelayWithSenderReports() func(r *relayPeer) {
	return func(r *relayPeer) {
		r.withSRReports = true
	}
}
