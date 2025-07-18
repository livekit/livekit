package rtc

import (
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/protocol/livekit"
)

func TestSignalFragment(t *testing.T) {
	inputMessage := &livekit.Envelope{
		ServerMessages: []*livekit.Signalv2ServerMessage{
			{
				Message: &livekit.Signalv2ServerMessage_ConnectResponse{
					ConnectResponse: &livekit.ConnectResponse{
						SifTrailer: []byte("abcdefghijklmnopqrstuvwxyz0123456789"),
					},
				},
			},
			{
				Message: &livekit.Signalv2ServerMessage_ConnectResponse{
					ConnectResponse: &livekit.ConnectResponse{
						SifTrailer: []byte("0123456789abcdefghijklmnopqrstuvwxyz0123456789"),
					},
				},
			},
			{
				Message: &livekit.Signalv2ServerMessage_ConnectResponse{
					ConnectResponse: &livekit.ConnectResponse{
						SifTrailer: []byte("ABCDEFGHIJKLMNOPQRSTabcdefghijklmnopqrstuvwxyz0123456789"),
					},
				},
			},
		},
	}

	t.Run("no segmentation needed", func(t *testing.T) {
		sr := NewSignalFragment(SignalFragmentParams{
			MaxFragmentSize: 5_000_000,
		})

		marshalled, err := proto.Marshal(inputMessage)
		require.NoError(t, err)
		require.Nil(t, sr.Segment(marshalled))
	})

	t.Run("segmentation + reassmebly", func(t *testing.T) {
		maxFragmentSize := 5
		sr := NewSignalFragment(SignalFragmentParams{
			MaxFragmentSize: maxFragmentSize,
		})

		marshalled, err := proto.Marshal(inputMessage)
		require.NoError(t, err)

		expectedNumFragments := (len(marshalled) + maxFragmentSize - 1) / maxFragmentSize

		fragments := sr.Segment(marshalled)
		require.NotZero(t, len(fragments))
		require.Equal(t, uint32(len(marshalled)), fragments[0].TotalSize)

		var reassembled []byte
		for idx, fragment := range fragments {
			require.Equal(t, uint32(idx+1), fragment.FragmentNumber)
			require.NotZero(t, fragment.FragmentSize)
			require.Equal(t, uint32(expectedNumFragments), fragment.NumFragments)
			require.Equal(t, fragment.FragmentSize, uint32(len(fragment.Data)))

			reassembled = sr.Reassemble(fragment)
		}
		require.Equal(t, marshalled, reassembled)
	})

	t.Run("runt", func(t *testing.T) {
		maxFragmentSize := 5
		sr := NewSignalFragment(SignalFragmentParams{
			MaxFragmentSize: maxFragmentSize,
		})

		marshalled, err := proto.Marshal(inputMessage)
		require.NoError(t, err)

		fragments := sr.Segment(marshalled)

		var reassembled []byte
		for idx, fragment := range fragments {
			// do not send one packet into re-assembly initially, re-assembly should not succeed
			if idx == 0 {
				continue
			}

			reassembled = sr.Reassemble(fragment)
		}
		require.Zero(t, len(reassembled))

		// submit 1st fragment and ensure reassembly completes
		reassembled = sr.Reassemble(fragments[0])
		require.Equal(t, marshalled, reassembled)
	})

	t.Run("corrupted", func(t *testing.T) {
		maxFragmentSize := 5
		sr := NewSignalFragment(SignalFragmentParams{
			MaxFragmentSize: maxFragmentSize,
		})

		marshalled, err := proto.Marshal(inputMessage)
		require.NoError(t, err)

		fragments := sr.Segment(marshalled)

		var reassembled []byte
		for idx, fragment := range fragments {
			// corrupt a fragment, re-assembly should fail
			if idx == 0 {
				fragment.FragmentSize += 1
			}

			reassembled = sr.Reassemble(fragment)
		}
		require.Zero(t, len(reassembled))
	})
}
