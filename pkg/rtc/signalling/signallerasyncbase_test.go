package signalling

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/atomic"

	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/routing/routingfakes"
	"github.com/livekit/livekit-server/pkg/rtc/types"
)

func newTestSignallerBase(onHandshakeOpened func()) *signallerAsyncBase {
	return newSignallerAsyncBase(signallerAsyncBaseParams{
		Logger:            logger.GetLogger(),
		OnHandshakeOpened: onHandshakeOpened,
	})
}

// currentHandshakeGeneration is what a timeout armed right now would be tagged with
func currentHandshakeGeneration(s *signallerAsyncBase) uint32 {
	s.resSinkMu.Lock()
	defer s.resSinkMu.Unlock()

	return s.handshakeGeneration
}

func TestHandshakeGate(t *testing.T) {
	t.Run("a resumed connection holds messages back", func(t *testing.T) {
		s := newTestSignallerBase(nil)
		require.False(t, s.HandshakePending())

		s.SwapResponseSink(&routingfakes.FakeMessageSink{}, types.SignallingCloseReasonResume)
		require.True(t, s.HandshakePending())

		s.OpenHandshake()
		require.False(t, s.HandshakePending())
	})

	t.Run("other sink swaps do not hold messages back", func(t *testing.T) {
		s := newTestSignallerBase(nil)

		s.SwapResponseSink(&routingfakes.FakeMessageSink{}, types.SignallingCloseReasonUnknown)
		require.False(t, s.HandshakePending())
	})

	t.Run("closing the connection clears the gate", func(t *testing.T) {
		s := newTestSignallerBase(nil)

		s.SwapResponseSink(&routingfakes.FakeMessageSink{}, types.SignallingCloseReasonResume)
		require.True(t, s.HandshakePending())

		s.CloseSignalConnection(types.SignallingCloseReasonParticipantClose)
		require.False(t, s.HandshakePending())
	})

	t.Run("the handshake window opens the gate", func(t *testing.T) {
		s := newTestSignallerBase(nil)

		s.SwapResponseSink(&routingfakes.FakeMessageSink{}, types.SignallingCloseReasonResume)
		require.True(t, s.HandshakePending())

		s.onHandshakeTimeout(currentHandshakeGeneration(s))
		require.False(t, s.HandshakePending())
	})

	t.Run("a swap to a fresh connection opens the gate", func(t *testing.T) {
		s := newTestSignallerBase(nil)

		s.SwapResponseSink(&routingfakes.FakeMessageSink{}, types.SignallingCloseReasonResume)
		require.True(t, s.HandshakePending())

		s.SwapResponseSink(&routingfakes.FakeMessageSink{}, types.SignallingCloseReasonUnknown)
		require.False(t, s.HandshakePending())
	})

	t.Run("a timeout of a wait that has ended does nothing", func(t *testing.T) {
		var opened atomic.Int32
		s := newTestSignallerBase(func() { opened.Inc() })

		s.SwapResponseSink(&routingfakes.FakeMessageSink{}, types.SignallingCloseReasonResume)
		stale := currentHandshakeGeneration(s)

		// the client resumes again while the first timeout is running, Stop cannot
		// cancel a callback that has already started
		s.SwapResponseSink(&routingfakes.FakeMessageSink{}, types.SignallingCloseReasonResume)
		s.onHandshakeTimeout(stale)

		require.True(t, s.HandshakePending())
		require.Zero(t, opened.Load())

		// and the wait in progress still opens on its own timeout
		s.onHandshakeTimeout(currentHandshakeGeneration(s))
		require.False(t, s.HandshakePending())
		require.EqualValues(t, 1, opened.Load())
	})
}

func TestHandshakeGateNotifiesOnOpen(t *testing.T) {
	t.Run("on an explicit open", func(t *testing.T) {
		var opened atomic.Int32
		s := newTestSignallerBase(func() { opened.Inc() })

		s.SwapResponseSink(&routingfakes.FakeMessageSink{}, types.SignallingCloseReasonResume)
		require.Zero(t, opened.Load())

		s.OpenHandshake()
		require.EqualValues(t, 1, opened.Load())

		// only the transition notifies
		s.OpenHandshake()
		require.EqualValues(t, 1, opened.Load())
	})

	t.Run("on the handshake window expiring", func(t *testing.T) {
		var opened atomic.Int32
		s := newTestSignallerBase(func() { opened.Inc() })

		s.SwapResponseSink(&routingfakes.FakeMessageSink{}, types.SignallingCloseReasonResume)
		s.onHandshakeTimeout(currentHandshakeGeneration(s))

		require.False(t, s.HandshakePending())
		require.EqualValues(t, 1, opened.Load())
	})

	t.Run("the window fires without anything else touching the gate", func(t *testing.T) {
		var opened atomic.Int32
		s := newTestSignallerBase(func() { opened.Inc() })

		s.SwapResponseSink(&routingfakes.FakeMessageSink{}, types.SignallingCloseReasonResume)
		require.Eventually(t, func() bool {
			return !s.HandshakePending() && opened.Load() == 1
		}, 2*handshakeWindow, 100*time.Millisecond)
	})

	t.Run("not on a connection close", func(t *testing.T) {
		var opened atomic.Int32
		s := newTestSignallerBase(func() { opened.Inc() })

		s.SwapResponseSink(&routingfakes.FakeMessageSink{}, types.SignallingCloseReasonResume)
		s.CloseSignalConnection(types.SignallingCloseReasonParticipantClose)

		require.False(t, s.HandshakePending())
		require.Zero(t, opened.Load())
	})

	t.Run("not when the gate was never armed", func(t *testing.T) {
		var opened atomic.Int32
		s := newTestSignallerBase(func() { opened.Inc() })

		s.OpenHandshake()
		require.Zero(t, opened.Load())
	})
}
