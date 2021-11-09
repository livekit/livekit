package buffer

import (
	"testing"

	"github.com/pion/rtp"
	"github.com/stretchr/testify/assert"
)

var TestPackets = []*rtp.Packet{
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

func Test_queue(t *testing.T) {
	b := make([]byte, 25000)
	q := NewBucket(&b)

	for _, p := range TestPackets {
		p := p
		buf, err := p.Marshal()
		assert.NoError(t, err)
		assert.NotPanics(t, func() {
			q.AddPacket(buf, p.SequenceNumber, true)
		})
	}
	var expectedSN uint16
	expectedSN = 6
	np := rtp.Packet{}
	buff := make([]byte, maxPktSize)
	i, err := q.GetPacket(buff, 6)
	assert.NoError(t, err)
	err = np.Unmarshal(buff[:i])
	assert.NoError(t, err)
	assert.Equal(t, expectedSN, np.SequenceNumber)

	np2 := &rtp.Packet{
		Header: rtp.Header{
			SequenceNumber: 8,
		},
	}
	buf, err := np2.Marshal()
	assert.NoError(t, err)
	expectedSN = 8
	q.AddPacket(buf, 8, false)
	i, err = q.GetPacket(buff, expectedSN)
	assert.NoError(t, err)
	err = np.Unmarshal(buff[:i])
	assert.NoError(t, err)
	assert.Equal(t, expectedSN, np.SequenceNumber)

	_, err = q.AddPacket(buf, 8, false)
	assert.ErrorIs(t, err, errRTXPacket)
}

func Test_queue_edges(t *testing.T) {
	var TestPackets = []*rtp.Packet{
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
		p := p
		assert.NotNil(t, p)
		assert.NotPanics(t, func() {
			p := p
			buf, err := p.Marshal()
			assert.NoError(t, err)
			assert.NotPanics(t, func() {
				q.AddPacket(buf, p.SequenceNumber, true)
			})
		})
	}
	var expectedSN uint16
	expectedSN = 65534
	np := rtp.Packet{}
	buff := make([]byte, maxPktSize)
	i, err := q.GetPacket(buff, expectedSN)
	assert.NoError(t, err)
	err = np.Unmarshal(buff[:i])
	assert.NoError(t, err)
	assert.Equal(t, expectedSN, np.SequenceNumber)

	np2 := rtp.Packet{
		Header: rtp.Header{
			SequenceNumber: 65535,
		},
	}
	buf, err := np2.Marshal()
	assert.NoError(t, err)
	q.AddPacket(buf, np2.SequenceNumber, false)
	i, err = q.GetPacket(buff, expectedSN+1)
	assert.NoError(t, err)
	err = np.Unmarshal(buff[:i])
	assert.NoError(t, err)
	assert.Equal(t, expectedSN+1, np.SequenceNumber)
}
