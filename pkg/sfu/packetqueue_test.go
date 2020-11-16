package sfu

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
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

func Test_queue_nack(t *testing.T) {
	type fields struct {
		headSN uint16
	}
	tests := []struct {
		name   string
		fields fields
		want   *rtcp.NackPair
	}{
		{
			name:   "Most generate correct nack packet",
			fields: fields{},
			want: &rtcp.NackPair{
				PacketID:    2,
				LostPackets: 100,
			},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			q := &queue{
				headSN: tt.fields.headSN,
			}
			for _, p := range TestPackets {
				diff := p.SequenceNumber - q.headSN
				for i := 1; i < int(diff); i++ {
					q.push(nil)
					q.counter++
				}
				q.headSN = p.SequenceNumber
				q.counter++
				q.push(p)
			}
			if got := q.nack(); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("nack() = %v, want %v", got, tt.want)
			}
		})
	}
}

func Test_queue(t *testing.T) {
	q := &queue{}
	for _, p := range TestPackets {
		p := p
		assert.NotPanics(t, func() {
			q.AddPacket(p, true)
		})
	}
	var expectedSN uint16
	expectedSN = 6
	assert.Equal(t, expectedSN, q.GetPacket(6).SequenceNumber)

	np := &rtp.Packet{
		Header: rtp.Header{
			SequenceNumber: 8,
		},
	}
	expectedSN = 8
	q.AddPacket(np, false)
	assert.Equal(t, expectedSN, q.GetPacket(8).SequenceNumber)
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
	q := &queue{}
	q.headSN = 65532
	for _, p := range TestPackets {
		p := p
		assert.NotPanics(t, func() {
			q.AddPacket(p, true)
		})
	}
	assert.Equal(t, 6, q.size)
	var expectedSN uint16
	expectedSN = 65534
	assert.Equal(t, expectedSN, q.GetPacket(expectedSN).SequenceNumber)

	np := &rtp.Packet{
		Header: rtp.Header{
			SequenceNumber: 65535,
		},
	}
	q.AddPacket(np, false)
	assert.Equal(t, expectedSN+1, q.GetPacket(expectedSN+1).SequenceNumber)
}
