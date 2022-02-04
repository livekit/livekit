package buffer

import (
	"testing"

	"github.com/pion/rtcp"
	"github.com/stretchr/testify/require"
)

func Test_nackQueue_pairs(t *testing.T) {
	tests := []struct {
		name string
		args []uint16
		want []rtcp.NackPair
	}{
		{
			name: "Must return correct single pairs pair",
			args: []uint16{1, 2, 4, 5},
			want: []rtcp.NackPair{{
				PacketID:    1,
				LostPackets: 13,
			}},
		},
		{
			name: "Must return correct pair wrap",
			args: []uint16{65533, 2, 4, 5, 30, 32},
			want: []rtcp.NackPair{
				{
					PacketID:    65533,
					LostPackets: 1<<7 + 1<<6 + 1<<4,
				},
				{
					PacketID:    30,
					LostPackets: 1 << 1,
				}},
		},
		{
			name: "Must return 2 pairs pair",
			args: []uint16{1, 2, 4, 5, 20, 22, 24, 27},
			want: []rtcp.NackPair{
				{
					PacketID:    1,
					LostPackets: 13,
				},
				{
					PacketID:    20,
					LostPackets: 74,
				},
			},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			n := NewNACKQueue()
			for _, sn := range tt.args {
				n.Push(sn)
			}
			got := n.Pairs()
			require.EqualValues(t, tt.want, got)
		})
	}
}

func Test_nackQueue_push(t *testing.T) {
	type args struct {
		sn []uint16
	}
	tests := []struct {
		name string
		args args
		want []uint16
	}{
		{
			name: "Must keep packet order",
			args: args{
				sn: []uint16{1, 3, 4, 5, 7, 8},
			},
			want: []uint16{1, 3, 4, 5, 7, 8},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			n := NewNACKQueue()
			for _, sn := range tt.args.sn {
				n.Push(sn)
			}
			var newSN []uint16
			for _, nack := range n.nacks {
				newSN = append(newSN, nack.seqNum)
			}
			require.Equal(t, tt.want, newSN)
		})
	}
}

func Test_nackQueue_remove(t *testing.T) {
	type args struct {
		sn []uint16
	}
	tests := []struct {
		name string
		args args
		want []uint16
	}{
		{
			name: "Must keep packet order",
			args: args{
				sn: []uint16{1, 3, 4, 5, 7, 8},
			},
			want: []uint16{1, 3, 4, 7, 8},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			n := NewNACKQueue()
			for _, sn := range tt.args.sn {
				n.Push(sn)
			}
			n.Remove(5)
			var newSN []uint16
			for _, nack := range n.nacks {
				newSN = append(newSN, nack.seqNum)
			}
			require.Equal(t, tt.want, newSN)
		})
	}
}
