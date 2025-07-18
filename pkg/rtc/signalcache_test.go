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
		// SIGNALLING-V2-TODO: replace with other kinds of messages when more types are added
		&livekit.Signalv2ServerMessage{
			Message: &livekit.Signalv2ServerMessage_ConnectResponse{},
		},
		&livekit.Signalv2ServerMessage{
			Message: &livekit.Signalv2ServerMessage_ConnectResponse{},
		},
		&livekit.Signalv2ServerMessage{
			Message: &livekit.Signalv2ServerMessage_ConnectResponse{},
		},
	}

	// Add() - add one message at a time
	for _, inputMessage := range inputMessages {
		cache.Add(inputMessage, 2345)
	}

	// get all messages in cache
	outputMessages := cache.GetFromFront()
	require.Equal(t, inputMessages, outputMessages)

	// clear one and get again
	cache.Clear(firstMessageId)

	outputMessages = cache.GetFromFront()
	require.Equal(t, inputMessages[1:], outputMessages)

	// clearing some evicted messages should not clear anything
	cache.Clear(firstMessageId) // firstMessageId has been cleared already at this point

	outputMessages = cache.GetFromFront()
	require.Equal(t, inputMessages[1:], outputMessages)

	// clear some and get rest in one go
	outputMessages = cache.ClearAndGetFrom(firstMessageId + 3)
	require.Equal(t, 1, len(outputMessages))
	require.Equal(t, inputMessages[3:], outputMessages)

	// getting again should get the same messages again as they sill should in cache
	outputMessages = cache.GetFromFront()
	require.Equal(t, inputMessages[3:], outputMessages)

	// clearing all and getting should return nil
	require.Nil(t, cache.ClearAndGetFrom(firstMessageId+uint32(len(inputMessages))))

	// getting again should return nil as the cache is fully cleared above
	require.Nil(t, cache.GetFromFront())

	// AddBatch() - add all messages at once
	cache.AddBatch(inputMessages, 4567)

	// get all messages in cache
	outputMessages = cache.GetFromFront()
	require.Equal(t, inputMessages, outputMessages)
}
