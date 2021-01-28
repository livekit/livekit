package test

import (
	"testing"
)

// a scenario with lots of clients connecting, publishing, and leaving at random periods
func scenarioRandom(t *testing.T) {

}

// websocket reconnects
func scenarioWSReconnect(t *testing.T) {
	c1 := createRTCClient("wsr_c1", defaultServerPort)
	c2 := createRTCClient("wsr_c2", defaultServerPort)

	waitUntilConnected(t, c1, c2)

	// c1 publishes track, but disconnects websockets and reconnects
}
