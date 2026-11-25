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
		packet, _ := a.push(&message.Datagram{Group: group, Object: index, EndOfGroup: end, Payload: payload}, 128)
		return packet
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
	push := func(d *message.Datagram) []byte { packet, _ := a.push(d, 128); return packet }
	chunk := bytes.Repeat([]byte{7}, dataChunkSize)
	for _, bad := range []*message.Datagram{
		{EndOfGroup: true}, {Payload: chunk[:1]}, {HasStatus: true, EndOfGroup: true, Payload: chunk},
		{Object: maxDataChunks, EndOfGroup: true, Payload: chunk}, {Object: maxDataChunks - 1, Payload: chunk},
		{EndOfGroup: true, Payload: make([]byte, dataChunkSize+1)},
	} {
		require.Nil(t, push(bad))
	}
	for i := maxDataChunks - 1; i > 0; i-- {
		require.Nil(t, push(&message.Datagram{Object: uint64(i), EndOfGroup: i == maxDataChunks-1, Payload: chunk}))
	}
	require.Equal(t, bytes.Repeat(chunk, maxDataChunks), push(&message.Datagram{Object: 0, Payload: chunk}))
}

func TestDataTrackPriorityScheduling(t *testing.T) {
	a := &dataDownload{ready: true, subscriberPriority: 128, queue: []queuedDataPacket{{priority: 255, sequence: 0}}}
	b := &dataDownload{ready: true, subscriberPriority: 128, queue: []queuedDataPacket{{priority: 0, sequence: 1}, {priority: 128, sequence: 3}}}
	c := &dataDownload{ready: true, subscriberPriority: 0, queue: []queuedDataPacket{{priority: 255, sequence: 2}}}
	d := &dataDownload{ready: true, subscriberPriority: 128, queue: []queuedDataPacket{{priority: 128, sequence: 4}}}
	unaccepted := &dataDownload{queue: []queuedDataPacket{{priority: 0}}}
	p := moqPeer{downloads: map[uint16]*dataDownload{1: a, 2: b, 3: c, 4: d, 5: unaccepted}}
	for _, want := range []struct {
		sub   *dataDownload
		group uint64
	}{{c, 0}, {b, 0}, {b, 1}, {d, 0}, {a, 0}} {
		sub, _, group := p.nextDataPacket()
		require.Same(t, want.sub, sub)
		require.Equal(t, want.group, group)
	}
	sub, _, _ := p.nextDataPacket()
	require.Nil(t, sub)
}

func TestDataTrackPriorityFromFirstChunk(t *testing.T) {
	for _, useDefault := range []bool{false, true} {
		var a dataAssembler
		partial, _ := a.push(&message.Datagram{Object: 1, Priority: 10, EndOfGroup: true, Payload: []byte{1}}, 255)
		require.Nil(t, partial)
		packet, priority := a.push(&message.Datagram{Object: 0, Priority: 0, DefaultPriority: useDefault, Payload: make([]byte, dataChunkSize)}, 255)
		require.Len(t, packet, dataChunkSize+1)
		if useDefault {
			require.EqualValues(t, 255, priority)
		} else {
			require.Zero(t, priority)
		}
	}
}
