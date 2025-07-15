package rtc

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/livekit"
)

func TestSignalCache(t *testing.T) {
	firstMessageId := uint32(10)
	cache := NewSignalCache(SignalCacheParams{
		FirstMessageId: firstMessageId,
	})

	inputMessages := []*livekit.Signalv2ServerMessage{
		&livekit.Signalv2ServerMessage{
			Message: &livekit.Signalv2ServerMessage_ConnectResponse{},
		},
		&livekit.Signalv2ServerMessage{
			Message: &livekit.Signalv2ServerMessage_Acknowledgement{},
		},
		&livekit.Signalv2ServerMessage{
			Message: &livekit.Signalv2ServerMessage_ConnectionQualityUpdate{},
		},
		&livekit.Signalv2ServerMessage{
			Message: &livekit.Signalv2ServerMessage_LeaveRequest{},
		},
	}

	for _, inputMessage := range inputMessages {
		// Add should return current message in envelope
		envelope := cache.Add(inputMessage, 2345)
		expectedEnvelope := &livekit.Signalv2ServerEnvelope{
			ServerMessages: []*livekit.Signalv2ServerMessage{inputMessage},
		}
		require.Equal(t, expectedEnvelope, envelope)
	}

	// get all messages in cache
	envelope := cache.GetFromFront()
	expectedEnvelope := &livekit.Signalv2ServerEnvelope{
		ServerMessages: inputMessages,
	}
	require.Equal(t, expectedEnvelope, envelope)

	// clear one and get again
	cache.Clear(firstMessageId)

	envelope = cache.GetFromFront()
	expectedEnvelope = &livekit.Signalv2ServerEnvelope{
		ServerMessages: inputMessages[1:],
	}
	require.Equal(t, expectedEnvelope, envelope)

	// clearing some evicted messages should not clear anything
	cache.Clear(firstMessageId) // firstMessageId has been cleared already at this point

	envelope = cache.GetFromFront()
	expectedEnvelope = &livekit.Signalv2ServerEnvelope{
		ServerMessages: inputMessages[1:],
	}
	require.Equal(t, expectedEnvelope, envelope)

	// clear some and get rest in one go
	envelope = cache.ClearAndGetFrom(firstMessageId + 3)
	expectedEnvelope = &livekit.Signalv2ServerEnvelope{
		ServerMessages: inputMessages[3:],
	}
	require.Equal(t, expectedEnvelope, envelope)

	// getting again should get the same messages again as they sill should in cache
	envelope = cache.GetFromFront()
	expectedEnvelope = &livekit.Signalv2ServerEnvelope{
		ServerMessages: inputMessages[3:],
	}
	require.Equal(t, expectedEnvelope, envelope)

	// clearing all and getting should return nil
	require.Nil(t, cache.ClearAndGetFrom(firstMessageId+uint32(len(inputMessages))))

	// getting again should return nil as the cache is fully cleared above
	require.Nil(t, cache.GetFromFront())
}
