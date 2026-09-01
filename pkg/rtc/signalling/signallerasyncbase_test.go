package signalling

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/routing/routingfakes"
	"github.com/livekit/livekit-server/pkg/rtc/types"
)

func TestHandshakeGate(t *testing.T) {
	newBase := func() *signallerAsyncBase {
		return newSignallerAsyncBase(signallerAsyncBaseParams{Logger: logger.GetLogger()})
	}

	t.Run("a resumed connection holds messages back", func(t *testing.T) {
		s := newBase()
		require.False(t, s.HandshakePending())

		s.SwapResponseSink(&routingfakes.FakeMessageSink{}, types.SignallingCloseReasonResume)
		require.True(t, s.HandshakePending())

		s.OpenHandshake()
		require.False(t, s.HandshakePending())
	})

	t.Run("other sink swaps do not hold messages back", func(t *testing.T) {
		s := newBase()

		s.SwapResponseSink(&routingfakes.FakeMessageSink{}, types.SignallingCloseReasonUnknown)
		require.False(t, s.HandshakePending())
	})

	t.Run("closing the connection opens the gate", func(t *testing.T) {
		s := newBase()

		s.SwapResponseSink(&routingfakes.FakeMessageSink{}, types.SignallingCloseReasonResume)
		require.True(t, s.HandshakePending())

		s.CloseSignalConnection(types.SignallingCloseReasonParticipantClose)
		require.False(t, s.HandshakePending())
	})

	t.Run("the gate opens on its own after the handshake window", func(t *testing.T) {
		s := newBase()

		s.SwapResponseSink(&routingfakes.FakeMessageSink{}, types.SignallingCloseReasonResume)
		require.True(t, s.HandshakePending())

		s.resSinkMu.Lock()
		s.handshakeArmedAt = time.Now().Add(-handshakeWindow - time.Second)
		s.resSinkMu.Unlock()

		require.False(t, s.HandshakePending())
		// and stays open
		require.False(t, s.HandshakePending())
	})
}

func TestHandshakeGateNotifiesOnOpen(t *testing.T) {
	var opened int
	newBase := func() *signallerAsyncBase {
		return newSignallerAsyncBase(signallerAsyncBaseParams{
			Logger:            logger.GetLogger(),
			OnHandshakeOpened: func() { opened++ },
		})
	}

	t.Run("on an explicit open", func(t *testing.T) {
		opened = 0
		s := newBase()

		s.SwapResponseSink(&routingfakes.FakeMessageSink{}, types.SignallingCloseReasonResume)
		require.Zero(t, opened)

		s.OpenHandshake()
		require.Equal(t, 1, opened)

		// only the transition notifies
		s.OpenHandshake()
		require.Equal(t, 1, opened)
	})

	t.Run("on the handshake window expiring", func(t *testing.T) {
		opened = 0
		s := newBase()

		s.SwapResponseSink(&routingfakes.FakeMessageSink{}, types.SignallingCloseReasonResume)
		s.resSinkMu.Lock()
		s.handshakeArmedAt = time.Now().Add(-handshakeWindow - time.Second)
		s.resSinkMu.Unlock()

		require.False(t, s.HandshakePending())
		require.Equal(t, 1, opened)

		require.False(t, s.HandshakePending())
		require.Equal(t, 1, opened)
	})

	t.Run("not when the gate was never armed", func(t *testing.T) {
		opened = 0
		s := newBase()

		s.OpenHandshake()
		require.Zero(t, opened)
	})
}
