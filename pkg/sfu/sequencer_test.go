package sfu

import (
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func Test_sequencer(t *testing.T) {
	seq := newSequencer(500)
	off := uint16(15)

	for i := uint16(1); i < 520; i++ {
		seq.push(i, i+off, 123, 2, true)
	}

	time.Sleep(60 * time.Millisecond)
	req := []uint16{57, 58, 62, 63, 513, 514, 515, 516, 517}
	res := seq.getSeqNoPairs(req)
	assert.Equal(t, len(req), len(res))
	for i, val := range res {
		assert.Equal(t, val.targetSeqNo, req[i])
		assert.Equal(t, val.sourceSeqNo, req[i]-off)
		assert.Equal(t, val.layer, uint8(2))
	}
	res = seq.getSeqNoPairs(req)
	assert.Equal(t, 0, len(res))
	time.Sleep(150 * time.Millisecond)
	res = seq.getSeqNoPairs(req)
	assert.Equal(t, len(req), len(res))
	for i, val := range res {
		assert.Equal(t, val.targetSeqNo, req[i])
		assert.Equal(t, val.sourceSeqNo, req[i]-off)
		assert.Equal(t, val.layer, uint8(2))
	}

	s := seq.push(521, 521+off, 123, 1, true)
	var (
		tlzIdx = uint8(15)
		picID  = uint16(16)
	)
	s.setVP8PayloadMeta(tlzIdx, picID)
	s.sourceSeqNo = 12
	m := seq.getSeqNoPairs([]uint16{521 + off})
	assert.Equal(t, 1, len(m))
	tlz0, pID := m[0].getVP8PayloadMeta()
	assert.Equal(t, tlzIdx, tlz0)
	assert.Equal(t, picID, pID)
}

func Test_sequencer_getNACKSeqNo(t *testing.T) {
	type args struct {
		seqNo []uint16
	}
	type fields struct {
		input  []uint16
		offset uint16
	}

	tests := []struct {
		name   string
		fields fields
		args   args
		want   []uint16
	}{
		{
			name: "Should get correct seq numbers",
			fields: fields{
				input:  []uint16{2, 3, 4, 7, 8},
				offset: 5,
			},
			args: args{
				seqNo: []uint16{4 + 5, 5 + 5, 8 + 5},
			},
			want: []uint16{4, 8},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			n := newSequencer(500)

			for _, i := range tt.fields.input {
				n.push(i, i+tt.fields.offset, 123, 3, true)
			}

			g := n.getSeqNoPairs(tt.args.seqNo)
			var got []uint16
			for _, sn := range g {
				got = append(got, sn.sourceSeqNo)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("getSeqNoPairs() = %v, want %v", got, tt.want)
			}
		})
	}
}
