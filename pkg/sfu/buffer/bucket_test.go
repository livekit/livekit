package buffer

import (
	"testing"

	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

func Test_queue(t *testing.T) {
	TestPackets := []*rtp.Packet{
		{
			Header: rtp.Header{
				SequenceNumber: 1,
			},
		},
		{
			Header: rtp.Header{
				SequenceNumber: 3,
			},
		},
		{
			Header: rtp.Header{
				SequenceNumber: 4,
			},
		},
		{
			Header: rtp.Header{
				SequenceNumber: 6,
			},
		},
		{
			Header: rtp.Header{
				SequenceNumber: 7,
			},
		},
		{
			Header: rtp.Header{
				SequenceNumber: 10,
			},
		},
	}

	b := make([]byte, 16000)
	q := NewBucket(&b)

	for _, p := range TestPackets {
		buf, err := p.Marshal()
		require.NoError(t, err)
		require.NotPanics(t, func() {
			_, _ = q.AddPacket(buf)
		})
	}

	expectedSN := uint16(6)
	np := rtp.Packet{}
	buff := make([]byte, maxPktSize)
	i, err := q.GetPacket(buff, expectedSN)
	require.NoError(t, err)
	err = np.Unmarshal(buff[:i])
	require.NoError(t, err)
	require.Equal(t, expectedSN, np.SequenceNumber)

	// add an out-of-order packet and ensure it can be retrieved
	np2 := &rtp.Packet{
		Header: rtp.Header{
			SequenceNumber: 8,
		},
	}
	buf, err := np2.Marshal()
	require.NoError(t, err)
	_, err = q.AddPacket(buf)
	require.NoError(t, err)
	expectedSN = 8
	i, err = q.GetPacket(buff, expectedSN)
	require.NoError(t, err)
	err = np.Unmarshal(buff[:i])
	require.NoError(t, err)
	require.Equal(t, expectedSN, np.SequenceNumber)

	_, err = q.AddPacket(buf)
	require.ErrorIs(t, err, ErrRTXPacket)

	// try to get old packets
	_, err = q.GetPacket(buff, 0)
	require.ErrorIs(t, err, ErrPacketNotFound)

	// ask for something ahead of headSN
	_, err = q.GetPacket(buff, 11)
	require.ErrorIs(t, err, ErrPacketNotFound)

	q.ResyncOnNextPacket()

	// should be able to get packets before adding a packet which will resync
	expectedSN = 8
	i, err = q.GetPacket(buff, expectedSN)
	require.NoError(t, err)
	err = np.Unmarshal(buff[:i])
	require.NoError(t, err)
	require.Equal(t, expectedSN, np.SequenceNumber)

	// adding a packet will resync and invalidate all existing
	buf, err = TestPackets[1].Marshal()
	require.NoError(t, err)
	_, err = q.AddPacket(buf)
	require.NoError(t, err)

	// try to get a valid packet before resync, should not be found
	_, err = q.GetPacket(buff, 8)
	require.ErrorIs(t, err, ErrPacketNotFound)

	// getting a packet added after resync should succeed
	expectedSN = TestPackets[1].Header.SequenceNumber
	i, err = q.GetPacket(buff, expectedSN)
	require.NoError(t, err)
	err = np.Unmarshal(buff[:i])
	require.NoError(t, err)
	require.Equal(t, expectedSN, np.SequenceNumber)
}

func Test_queue_edges(t *testing.T) {
	TestPackets := []*rtp.Packet{
		{
			Header: rtp.Header{
				SequenceNumber: 65533,
			},
		},
		{
			Header: rtp.Header{
				SequenceNumber: 65534,
			},
		},
		{
			Header: rtp.Header{
				SequenceNumber: 2,
			},
		},
	}

	b := make([]byte, 25000)
	q := NewBucket(&b)

	for _, p := range TestPackets {
		require.NotPanics(t, func() {
			buf, err := p.Marshal()
			require.NoError(t, err)
			require.NotPanics(t, func() {
				_, _ = q.AddPacket(buf)
			})
		})
	}

	expectedSN := uint16(65534)
	np := rtp.Packet{}
	buff := make([]byte, maxPktSize)
	i, err := q.GetPacket(buff, expectedSN)
	require.NoError(t, err)
	err = np.Unmarshal(buff[:i])
	require.NoError(t, err)
	require.Equal(t, expectedSN, np.SequenceNumber)

	// add an out-of-order packet where the head sequence has wrapped and ensure it can be retrieved
	np2 := rtp.Packet{
		Header: rtp.Header{
			SequenceNumber: 65535,
		},
	}
	buf, err := np2.Marshal()
	require.NoError(t, err)
	_, _ = q.AddPacket(buf)
	i, err = q.GetPacket(buff, expectedSN+1)
	require.NoError(t, err)
	err = np.Unmarshal(buff[:i])
	require.NoError(t, err)
	require.Equal(t, expectedSN+1, np.SequenceNumber)
}

func Test_queue_wrap(t *testing.T) {
	TestPackets := []*rtp.Packet{
		{
			Header: rtp.Header{
				SequenceNumber: 1,
			},
		},
		{
			Header: rtp.Header{
				SequenceNumber: 3,
			},
		},
		{
			Header: rtp.Header{
				SequenceNumber: 4,
			},
		},
		{
			Header: rtp.Header{
				SequenceNumber: 6,
			},
		},
		{
			Header: rtp.Header{
				SequenceNumber: 7,
			},
		},
		{
			Header: rtp.Header{
				SequenceNumber: 10,
			},
		},
		{
			Header: rtp.Header{
				SequenceNumber: 13,
			},
		},
		{
			Header: rtp.Header{
				SequenceNumber: 15,
			},
		},
	}

	b := make([]byte, 16000)
	q := NewBucket(&b)

	for _, p := range TestPackets {
		buf, err := p.Marshal()
		require.NoError(t, err)
		require.NotPanics(t, func() {
			_, _ = q.AddPacket(buf)
		})
	}

	buff := make([]byte, maxPktSize)

	// try to get old packets, but were valid before the bucket wrapped
	_, err := q.GetPacket(buff, 1)
	require.ErrorIs(t, err, ErrPacketNotFound)
	_, err = q.GetPacket(buff, 3)
	require.ErrorIs(t, err, ErrPacketNotFound)
	_, err = q.GetPacket(buff, 4)
	require.ErrorIs(t, err, ErrPacketNotFound)

	expectedSN := uint16(6)
	np := rtp.Packet{}
	i, err := q.GetPacket(buff, expectedSN)
	require.NoError(t, err)
	err = np.Unmarshal(buff[:i])
	require.NoError(t, err)
	require.Equal(t, expectedSN, np.SequenceNumber)

	// add an out-of-order packet and ensure it can be retrieved
	np2 := &rtp.Packet{
		Header: rtp.Header{
			SequenceNumber: 8,
		},
	}
	buf, err := np2.Marshal()
	require.NoError(t, err)
	_, err = q.AddPacket(buf)
	require.NoError(t, err)
	expectedSN = 8
	i, err = q.GetPacket(buff, expectedSN)
	require.NoError(t, err)
	err = np.Unmarshal(buff[:i])
	require.NoError(t, err)
	require.Equal(t, expectedSN, np.SequenceNumber)

	// add a packet with a large gap in sequence number which will invalidate all the slots
	np3 := &rtp.Packet{
		Header: rtp.Header{
			SequenceNumber: 56,
		},
	}
	buf, err = np3.Marshal()
	require.NoError(t, err)
	_, err = q.AddPacket(buf)
	require.NoError(t, err)
	expectedSN = 56
	i, err = q.GetPacket(buff, expectedSN)
	require.NoError(t, err)
	err = np.Unmarshal(buff[:i])
	require.NoError(t, err)
	require.Equal(t, expectedSN, np.SequenceNumber)

	// after the large jump invalidating all slots, retrieving previously added packets should fail
	_, err = q.GetPacket(buff, 6)
	require.ErrorIs(t, err, ErrPacketNotFound)
	_, err = q.GetPacket(buff, 7)
	require.ErrorIs(t, err, ErrPacketNotFound)
	_, err = q.GetPacket(buff, 8)
	require.ErrorIs(t, err, ErrPacketNotFound)
	_, err = q.GetPacket(buff, 10)
	require.ErrorIs(t, err, ErrPacketNotFound)
	_, err = q.GetPacket(buff, 13)
	require.ErrorIs(t, err, ErrPacketNotFound)
	_, err = q.GetPacket(buff, 15)
	require.ErrorIs(t, err, ErrPacketNotFound)
}
