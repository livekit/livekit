// Copyright 2026 LiveKit, Inc.
// SPDX-License-Identifier: Apache-2.0

package moq

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/trickstercache/trickster/v2/pkg/moq/message"
)

func TestDataTrackDatagramLossAndReordering(t *testing.T) {
	var a dataAssembler
	push := func(group, index uint64, end bool, payload []byte) []byte {
		return a.push(&message.Datagram{Group: group, Object: index, EndOfGroup: end, Payload: payload})
	}
	first := bytes.Repeat([]byte{1}, dataChunkSize)
	last := []byte{2, 3}
	require.Nil(t, push(0, 0, false, first))
	require.Nil(t, push(1, 1, true, last))
	require.Nil(t, push(1, 1, true, last))
	require.Equal(t, append(bytes.Clone(first), last...), push(1, 0, false, first))
	require.Nil(t, push(1, 0, false, first))
	require.Equal(t, last, push(dataGroupWindow, 0, true, last))
	require.Nil(t, push(0, 1, true, last))
}

func TestDataTrackDatagramBounds(t *testing.T) {
	var a dataAssembler
	chunk := bytes.Repeat([]byte{7}, dataChunkSize)
	for _, bad := range []*message.Datagram{
		{EndOfGroup: true}, {Payload: chunk[:1]}, {HasStatus: true, EndOfGroup: true, Payload: chunk},
		{Object: maxDataChunks, EndOfGroup: true, Payload: chunk}, {Object: maxDataChunks - 1, Payload: chunk},
		{EndOfGroup: true, Payload: make([]byte, dataChunkSize+1)},
	} {
		require.Nil(t, a.push(bad))
	}
	for i := maxDataChunks - 1; i > 0; i-- {
		require.Nil(t, a.push(&message.Datagram{Object: uint64(i), EndOfGroup: i == maxDataChunks-1, Payload: chunk}))
	}
	require.Equal(t, bytes.Repeat(chunk, maxDataChunks), a.push(&message.Datagram{Object: 0, Payload: chunk}))
}
