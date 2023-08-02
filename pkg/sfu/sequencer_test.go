// Copyright 2023 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package sfu

import (
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/logger"
)

func Test_sequencer(t *testing.T) {
	seq := newSequencer(500, 0, logger.GetLogger())
	off := uint16(15)

	for i := uint16(1); i < 518; i++ {
		seq.push(i, i+off, 123, true, 2, nil, nil)
	}
	// send the last two out-of-order
	seq.push(519, 519+off, 123, false, 2, nil, nil)
	seq.push(518, 518+off, 123, true, 2, nil, nil)

	req := []uint16{57, 58, 62, 63, 513, 514, 515, 516, 517}
	res := seq.getPacketsMeta(req)
	// nothing should be returned as not enough time has elapsed since sending packet
	require.Equal(t, 0, len(res))

	time.Sleep((ignoreRetransmission + 10) * time.Millisecond)
	res = seq.getPacketsMeta(req)
	require.Equal(t, len(req), len(res))
	for i, val := range res {
		require.Equal(t, val.targetSeqNo, req[i])
		require.Equal(t, val.sourceSeqNo, req[i]-off)
		require.Equal(t, val.layer, int8(2))
	}
	res = seq.getPacketsMeta(req)
	require.Equal(t, 0, len(res))
	time.Sleep((ignoreRetransmission + 10) * time.Millisecond)
	res = seq.getPacketsMeta(req)
	require.Equal(t, len(req), len(res))
	for i, val := range res {
		require.Equal(t, val.targetSeqNo, req[i])
		require.Equal(t, val.sourceSeqNo, req[i]-off)
		require.Equal(t, val.layer, int8(2))
	}

	seq.push(521, 521+off, 123, true, 1, nil, nil)
	m := seq.getPacketsMeta([]uint16{521 + off})
	require.Equal(t, 0, len(m))
	time.Sleep((ignoreRetransmission + 10) * time.Millisecond)
	m = seq.getPacketsMeta([]uint16{521 + off})
	require.Equal(t, 1, len(m))

	seq.push(505, 505+off, 123, false, 1, nil, nil)
	m = seq.getPacketsMeta([]uint16{505 + off})
	require.Equal(t, 0, len(m))
	time.Sleep((ignoreRetransmission + 10) * time.Millisecond)
	m = seq.getPacketsMeta([]uint16{505 + off})
	require.Equal(t, 1, len(m))
}

func Test_sequencer_getNACKSeqNo(t *testing.T) {
	type args struct {
		seqNo []uint16
	}
	type fields struct {
		input          []uint16
		padding        []uint16
		offset         uint16
		markerOdd      bool
		markerEven     bool
		codecBytesOdd  []byte
		codecBytesEven []byte
		ddBytesOdd     []byte
		ddBytesEven    []byte
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
				input:          []uint16{2, 3, 4, 7, 8, 11},
				padding:        []uint16{9, 10},
				offset:         5,
				markerOdd:      true,
				markerEven:     false,
				codecBytesOdd:  []byte{1, 2, 3, 4},
				codecBytesEven: []byte{5, 6, 7},
				ddBytesOdd:     []byte{8, 9, 10},
				ddBytesEven:    []byte{11, 12},
			},
			args: args{
				seqNo: []uint16{4 + 5, 5 + 5, 8 + 5, 9 + 5, 10 + 5, 11 + 5},
			},
			want: []uint16{4, 8, 11},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			n := newSequencer(5, 10, logger.GetLogger())

			for _, i := range tt.fields.input {
				if i%2 == 0 {
					n.push(i, i+tt.fields.offset, 123, tt.fields.markerEven, 3, tt.fields.codecBytesEven, tt.fields.ddBytesEven)
				} else {
					n.push(i, i+tt.fields.offset, 123, tt.fields.markerOdd, 3, tt.fields.codecBytesOdd, tt.fields.ddBytesOdd)
				}
			}
			for _, i := range tt.fields.padding {
				n.pushPadding(i + tt.fields.offset)
			}

			time.Sleep((ignoreRetransmission + 10) * time.Millisecond)
			g := n.getPacketsMeta(tt.args.seqNo)
			var got []uint16
			for _, sn := range g {
				got = append(got, sn.sourceSeqNo)
				if sn.sourceSeqNo%2 == 0 {
					require.Equal(t, tt.fields.markerEven, sn.marker)
					require.Equal(t, tt.fields.codecBytesEven, sn.codecBytes)
					require.Equal(t, tt.fields.ddBytesEven, sn.ddBytes)
				} else {
					require.Equal(t, tt.fields.markerOdd, sn.marker)
					require.Equal(t, tt.fields.codecBytesOdd, sn.codecBytes)
					require.Equal(t, tt.fields.ddBytesOdd, sn.ddBytes)
				}
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("getPacketsMeta() = %v, want %v", got, tt.want)
			}
		})
	}
}
