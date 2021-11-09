package buffer

import (
	"math/rand"
	"reflect"
	"testing"
	"time"

	"github.com/pion/rtcp"
	"github.com/stretchr/testify/assert"
)

func Test_nackQueue_pairs(t *testing.T) {
	type fields struct {
		nacks []nack
	}
	tests := []struct {
		name   string
		fields fields
		args   []uint32
		want   []rtcp.NackPair
	}{
		{
			name: "Must return correct single pairs pair",
			fields: fields{
				nacks: nil,
			},
			args: []uint32{1, 2, 4, 5},
			want: []rtcp.NackPair{{
				PacketID:    1,
				LostPackets: 13,
			}},
		},
		{
			name: "Must return correct pair wrap",
			fields: fields{
				nacks: nil,
			},
			args: []uint32{65536, 65538, 65540, 65541, 65566, 65568}, // wrap around 65533,2,4,5
			want: []rtcp.NackPair{{
				PacketID:    0, // 65536
				LostPackets: 1<<4 + 1<<3 + 1<<1,
			},
				{
					PacketID:    30, // 65566
					LostPackets: 1 << 1,
				}},
		},
		{
			name: "Must return 2 pairs pair",
			fields: fields{
				nacks: nil,
			},
			args: []uint32{1, 2, 4, 5, 20, 22, 24, 27},
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
			n := &nackQueue{
				nacks: tt.fields.nacks,
			}
			for _, sn := range tt.args {
				n.push(sn)
			}
			got, _ := n.pairs(75530)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("pairs() = %v, want %v", got, tt.want)
			}
		})
	}
}

func Test_nackQueue_push(t *testing.T) {
	type fields struct {
		nacks []nack
	}
	type args struct {
		sn []uint32
	}
	tests := []struct {
		name   string
		fields fields
		args   args
		want   []uint32
	}{
		{
			name: "Must keep packet order",
			fields: fields{
				nacks: make([]nack, 0, 10),
			},
			args: args{
				sn: []uint32{3, 4, 1, 5, 8, 7, 5},
			},
			want: []uint32{1, 3, 4, 5, 7, 8},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			n := &nackQueue{
				nacks: tt.fields.nacks,
			}
			for _, sn := range tt.args.sn {
				n.push(sn)
			}
			var newSN []uint32
			for _, sn := range n.nacks {
				newSN = append(newSN, sn.sn)
			}
			assert.Equal(t, tt.want, newSN)
		})
	}
}

func Test_nackQueue(t *testing.T) {
	type fields struct {
		nacks []nack
	}
	type args struct {
		sn []uint32
	}
	tests := []struct {
		name   string
		fields fields
		args   args
	}{
		{
			name: "Must keep packet order",
			fields: fields{
				nacks: make([]nack, 0, 10),
			},
			args: args{
				sn: []uint32{3, 4, 1, 5, 8, 7, 5},
			},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			n := nackQueue{}
			r := rand.New(rand.NewSource(time.Now().UnixNano()))
			for i := 0; i < 100; i++ {
				assert.NotPanics(t, func() {
					n.push(uint32(r.Intn(60000)))
					n.remove(uint32(r.Intn(60000)))
					n.pairs(60001)
				})
			}
		})
	}
}

func Test_nackQueue_remove(t *testing.T) {
	type args struct {
		sn []uint32
	}
	tests := []struct {
		name string
		args args
		want []uint32
	}{
		{
			name: "Must keep packet order",
			args: args{
				sn: []uint32{3, 4, 1, 5, 8, 7, 5},
			},
			want: []uint32{1, 3, 4, 7, 8},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			n := nackQueue{}
			for _, sn := range tt.args.sn {
				n.push(sn)
			}
			n.remove(5)
			var newSN []uint32
			for _, sn := range n.nacks {
				newSN = append(newSN, sn.sn)
			}
			assert.Equal(t, tt.want, newSN)
		})
	}
}
