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
	seq := newSequencer(500, false, logger.GetLogger())
	off := uint16(15)

	for i := uint64(1); i < 518; i++ {
		seq.push(time.Now().UnixNano(), i, i+uint64(off), 123, true, 2, nil, 0, nil, nil)
	}
	// send the last two out-of-order
	seq.push(time.Now().UnixNano(), 519, 519+uint64(off), 123, false, 2, nil, 0, nil, nil)
	seq.push(time.Now().UnixNano(), 518, 518+uint64(off), 123, true, 2, nil, 0, nil, nil)

	req := []uint16{57, 58, 62, 63, 513, 514, 515, 516, 517}
	res := seq.getExtPacketMetas(req)
	// nothing should be returned as not enough time has elapsed since sending packet
	require.Equal(t, 0, len(res))

	time.Sleep((ignoreRetransmission + 10) * time.Millisecond)
	res = seq.getExtPacketMetas(req)
	require.Equal(t, len(req), len(res))
	for i, val := range res {
		require.Equal(t, val.targetSeqNo, req[i])
		require.Equal(t, val.sourceSeqNo, uint64(req[i]-off))
		require.Equal(t, val.layer, int8(2))
		require.Equal(t, val.extSequenceNumber, uint64(req[i]))
		require.Equal(t, val.extTimestamp, uint64(123))
	}
	res = seq.getExtPacketMetas(req)
	require.Equal(t, 0, len(res))
	time.Sleep((ignoreRetransmission + 10) * time.Millisecond)
	res = seq.getExtPacketMetas(req)
	require.Equal(t, len(req), len(res))
	for i, val := range res {
		require.Equal(t, val.targetSeqNo, req[i])
		require.Equal(t, val.sourceSeqNo, uint64(req[i]-off))
		require.Equal(t, val.layer, int8(2))
		require.Equal(t, val.extSequenceNumber, uint64(req[i]))
		require.Equal(t, val.extTimestamp, uint64(123))
	}

	seq.push(time.Now().UnixNano(), 521, 521+uint64(off), 123, true, 1, nil, 0, nil, nil)
	m := seq.getExtPacketMetas([]uint16{521 + off})
	require.Equal(t, 0, len(m))
	time.Sleep((ignoreRetransmission + 10) * time.Millisecond)
	m = seq.getExtPacketMetas([]uint16{521 + off})
	require.Equal(t, 1, len(m))

	seq.push(time.Now().UnixNano(), 505, 505+uint64(off), 123, false, 1, nil, 0, nil, nil)
	m = seq.getExtPacketMetas([]uint16{505 + off})
	require.Equal(t, 0, len(m))
	time.Sleep((ignoreRetransmission + 10) * time.Millisecond)
	m = seq.getExtPacketMetas([]uint16{505 + off})
	require.Equal(t, 1, len(m))
}

func Test_sequencer_getNACKSeqNo_exclusion(t *testing.T) {
	type args struct {
		seqNo []uint16
	}
	type input struct {
		seqNo     uint64
		isPadding bool
	}
	type fields struct {
		inputs              []input
		offset              uint64
		markerOdd           bool
		markerEven          bool
		codecBytesOdd       []byte
		numCodecBytesInOdd  int
		codecBytesEven      []byte
		numCodecBytesInEven int
		codecBytesOversized []byte
		ddBytesOdd          []byte
		ddBytesEven         []byte
		ddBytesOversized    []byte
		actBytesOdd         []byte
		actBytesEven        []byte
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
				inputs: []input{
					{65526, false},
					{65524, false},
					{65525, false},
					{65529, false},
					{65530, false},
					{65531, true},
					{65533, false},
					{65532, true},
					{65534, false},
				},
				offset:              5,
				markerOdd:           true,
				markerEven:          false,
				codecBytesOdd:       []byte{1, 2, 3, 4},
				numCodecBytesInOdd:  3,
				codecBytesEven:      []byte{5, 6, 7},
				numCodecBytesInEven: 4,
				codecBytesOversized: []byte{1, 2, 3, 4, 5, 6, 7, 8, 9},
				ddBytesOdd:          []byte{8, 9, 10},
				ddBytesEven:         []byte{11, 12},
				ddBytesOversized:    []byte{11, 12, 13, 14, 15, 16, 17, 18, 19},
				actBytesOdd:         []byte{0, 1, 2, 3, 4, 5, 6, 7},
				actBytesEven:        []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15},
			},
			args: args{
				seqNo: []uint16{65526 + 5, 65527 + 5, 65530 + 5, 0 /* 65531 input */, 1 /* 65532 input */, 2 /* 65533 input */, 3 /* 65534 input */},
			},
			// although 65526 is originally pushed, that would have been reset by 65532 (padding only packet)
			// because of trying to add an exclusion range before highest sequence number which will fail
			// and the resulting fix up of the exclusion range slots
			want: []uint16{65530, 65533, 65534},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			n := newSequencer(5, true, logger.GetLogger())

			for _, i := range tt.fields.inputs {
				if i.isPadding {
					n.pushPadding(i.seqNo+tt.fields.offset, i.seqNo+tt.fields.offset)
				} else {
					if i.seqNo%5 == 0 {
						n.push(
							time.Now().UnixNano(),
							i.seqNo,
							i.seqNo+tt.fields.offset,
							123,
							tt.fields.markerOdd,
							3,
							tt.fields.codecBytesOversized,
							len(tt.fields.codecBytesOversized),
							tt.fields.ddBytesOversized,
							tt.fields.actBytesOdd,
						)
					} else {
						if i.seqNo%2 == 0 {
							n.push(
								time.Now().UnixNano(),
								i.seqNo,
								i.seqNo+tt.fields.offset,
								123,
								tt.fields.markerEven,
								3,
								tt.fields.codecBytesEven,
								tt.fields.numCodecBytesInEven,
								tt.fields.ddBytesEven,
								tt.fields.actBytesEven,
							)
						} else {
							n.push(
								time.Now().UnixNano(),
								i.seqNo,
								i.seqNo+tt.fields.offset,
								123,
								tt.fields.markerOdd,
								3,
								tt.fields.codecBytesOdd,
								tt.fields.numCodecBytesInOdd,
								tt.fields.ddBytesOdd,
								tt.fields.actBytesOdd,
							)
						}
					}
				}
			}

			time.Sleep((ignoreRetransmission + 10) * time.Millisecond)
			g := n.getExtPacketMetas(tt.args.seqNo)
			var got []uint16
			for _, sn := range g {
				got = append(got, uint16(sn.sourceSeqNo))
				if sn.sourceSeqNo%5 == 0 {
					require.Equal(t, tt.fields.markerOdd, sn.marker)
					require.Equal(t, tt.fields.codecBytesOversized, sn.codecBytesSlice)
					require.Equal(t, uint8(len(tt.fields.codecBytesOversized)), sn.numCodecBytesIn)
					require.Equal(t, tt.fields.ddBytesOversized, sn.ddBytesSlice)
					require.Equal(t, uint8(len(tt.fields.codecBytesOversized)), sn.ddBytesSize)
					require.Equal(t, tt.fields.actBytesOdd, sn.actBytes)
				} else {
					if sn.sourceSeqNo%2 == 0 {
						require.Equal(t, tt.fields.markerEven, sn.marker)
						require.Equal(t, tt.fields.codecBytesEven, sn.codecBytes[:sn.numCodecBytesOut])
						require.Equal(t, uint8(tt.fields.numCodecBytesInEven), sn.numCodecBytesIn)
						require.Equal(t, tt.fields.ddBytesEven, sn.ddBytes[:sn.ddBytesSize])
						require.Equal(t, uint8(len(tt.fields.ddBytesEven)), sn.ddBytesSize)
						require.Equal(t, tt.fields.actBytesEven, sn.actBytes)
					} else {
						require.Equal(t, tt.fields.markerOdd, sn.marker)
						require.Equal(t, tt.fields.codecBytesOdd, sn.codecBytes[:sn.numCodecBytesOut])
						require.Equal(t, uint8(tt.fields.numCodecBytesInOdd), sn.numCodecBytesIn)
						require.Equal(t, tt.fields.ddBytesOdd, sn.ddBytes[:sn.ddBytesSize])
						require.Equal(t, uint8(len(tt.fields.ddBytesOdd)), sn.ddBytesSize)
						require.Equal(t, tt.fields.actBytesOdd, sn.actBytes)
					}
				}
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("getExtPacketMetas() = %v, want %v", got, tt.want)
			}
		})
	}
}

func Test_sequencer_getNACKSeqNo_no_exclusion(t *testing.T) {
	type args struct {
		seqNo []uint16
	}
	type input struct {
		seqNo     uint64
		isPadding bool
	}
	type fields struct {
		inputs              []input
		offset              uint64
		markerOdd           bool
		markerEven          bool
		codecBytesOdd       []byte
		numCodecBytesInOdd  int
		codecBytesEven      []byte
		numCodecBytesInEven int
		ddBytesOdd          []byte
		ddBytesEven         []byte
		actBytesOdd         []byte
		actBytesEven        []byte
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
				inputs: []input{
					{2, false},
					{3, false},
					{4, false},
					{7, false},
					{8, false},
					{9, true},
					{11, false},
					{10, true},
					{12, false},
					{13, false},
				},
				offset:              5,
				markerOdd:           true,
				markerEven:          false,
				codecBytesOdd:       []byte{1, 2, 3, 4},
				numCodecBytesInOdd:  3,
				codecBytesEven:      []byte{5, 6, 7},
				numCodecBytesInEven: 4,
				ddBytesOdd:          []byte{8, 9, 10},
				ddBytesEven:         []byte{11, 12},
				actBytesOdd:         []byte{8, 9, 10},
				actBytesEven:        []byte{11, 12},
			},
			args: args{
				seqNo: []uint16{4 + 5, 5 + 5, 8 + 5, 9 + 5, 10 + 5, 11 + 5, 12 + 5},
			},
			// although 4 and 8 were originally added, they would be too old after a cycle of sequencer buffer
			want: []uint16{11, 12},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			n := newSequencer(5, false, logger.GetLogger())

			for _, i := range tt.fields.inputs {
				if i.isPadding {
					n.pushPadding(i.seqNo+tt.fields.offset, i.seqNo+tt.fields.offset)
				} else {
					if i.seqNo%2 == 0 {
						n.push(
							time.Now().UnixNano(),
							i.seqNo,
							i.seqNo+tt.fields.offset,
							123,
							tt.fields.markerEven,
							3,
							tt.fields.codecBytesEven,
							tt.fields.numCodecBytesInEven,
							tt.fields.ddBytesEven,
							tt.fields.actBytesEven,
						)
					} else {
						n.push(
							time.Now().UnixNano(),
							i.seqNo,
							i.seqNo+tt.fields.offset,
							123,
							tt.fields.markerOdd,
							3,
							tt.fields.codecBytesOdd,
							tt.fields.numCodecBytesInOdd,
							tt.fields.ddBytesOdd,
							tt.fields.actBytesOdd,
						)
					}
				}
			}

			time.Sleep((ignoreRetransmission + 10) * time.Millisecond)
			g := n.getExtPacketMetas(tt.args.seqNo)
			var got []uint16
			for _, sn := range g {
				got = append(got, uint16(sn.sourceSeqNo))
				if sn.sourceSeqNo%2 == 0 {
					require.Equal(t, tt.fields.markerEven, sn.marker)
					require.Equal(t, tt.fields.codecBytesEven, sn.codecBytes[:sn.numCodecBytesOut])
					require.Equal(t, uint8(tt.fields.numCodecBytesInEven), sn.numCodecBytesIn)
					require.Equal(t, tt.fields.ddBytesEven, sn.ddBytes[:sn.ddBytesSize])
					require.Equal(t, uint8(len(tt.fields.ddBytesEven)), sn.ddBytesSize)
					require.Equal(t, tt.fields.actBytesEven, sn.actBytes)
				} else {
					require.Equal(t, tt.fields.markerOdd, sn.marker)
					require.Equal(t, tt.fields.codecBytesOdd, sn.codecBytes[:sn.numCodecBytesOut])
					require.Equal(t, uint8(tt.fields.numCodecBytesInOdd), sn.numCodecBytesIn)
					require.Equal(t, tt.fields.ddBytesOdd, sn.ddBytes[:sn.ddBytesSize])
					require.Equal(t, uint8(len(tt.fields.ddBytesOdd)), sn.ddBytesSize)
					require.Equal(t, tt.fields.actBytesOdd, sn.actBytes)
				}
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("getExtPacketMetas() = %v, want %v", got, tt.want)
			}
		})
	}
}
