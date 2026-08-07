package agent

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/livekit"
)

type fakeSignalConn struct{}

func (fakeSignalConn) WriteServerMessage(msg *livekit.ServerMessage) (int, error) { return 0, nil }
func (fakeSignalConn) ReadWorkerMessage() (*livekit.WorkerMessage, int, error)    { return nil, 0, nil }
func (fakeSignalConn) SetReadDeadline(time.Time) error                            { return nil }
func (fakeSignalConn) Close() error                                               { return nil }
func (fakeSignalConn) CloseWithReason(reason string) error                        { return nil }

func TestRegisterAgentNameAllowlist(t *testing.T) {
	newRegisterer := func(allowed []string) *WorkerRegisterer {
		base := MakeWorkerRegistration()
		base.AllowedAgentNames = allowed
		return NewWorkerRegisterer(fakeSignalConn{}, &livekit.ServerInfo{}, base)
	}

	t.Run("nil allowlist permits any name (backwards compatible)", func(t *testing.T) {
		h := newRegisterer(nil)
		require.NoError(t, h.HandleRegister(&livekit.RegisterWorkerRequest{
			Type:      livekit.JobType_JT_ROOM,
			AgentName: "anything",
		}))
		require.Equal(t, "anything", h.Registration().AgentName)
	})

	t.Run("allowed name registers", func(t *testing.T) {
		h := newRegisterer([]string{"telephony", "translation"})
		require.NoError(t, h.HandleRegister(&livekit.RegisterWorkerRequest{
			Type:      livekit.JobType_JT_ROOM,
			AgentName: "telephony",
		}))
	})

	t.Run("unlisted name is rejected", func(t *testing.T) {
		h := newRegisterer([]string{"telephony"})
		err := h.HandleRegister(&livekit.RegisterWorkerRequest{
			Type:      livekit.JobType_JT_ROOM,
			AgentName: "translation",
		})
		require.ErrorIs(t, err, ErrAgentNameNotAllowed)
	})

	t.Run("empty allowlist rejects every name (key pinned to no agents)", func(t *testing.T) {
		h := newRegisterer([]string{})
		err := h.HandleRegister(&livekit.RegisterWorkerRequest{
			Type:      livekit.JobType_JT_ROOM,
			AgentName: "translation",
		})
		require.ErrorIs(t, err, ErrAgentNameNotAllowed)
	})
}
