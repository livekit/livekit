package twcc

import (
	"math/rand"
	"testing"
	"time"

	"github.com/pion/rtcp"
	"github.com/stretchr/testify/assert"
)

func TestTransportWideCC_writeRunLengthChunk(t1 *testing.T) {
	type fields struct {
		len uint16
	}
	type args struct {
		symbol    uint16
		runLength uint16
	}
	tests := []struct {
		name      string
		fields    fields
		args      args
		wantErr   bool
		wantBytes []byte
	}{
		{
			name: "Must not return error",

			args: args{
				symbol:    rtcp.TypeTCCPacketNotReceived,
				runLength: 221,
			},
			wantErr:   false,
			wantBytes: []byte{0, 0xdd},
		}, {
			name: "Must set run length after padding",
			fields: fields{
				len: 1,
			},
			args: args{
				symbol:    rtcp.TypeTCCPacketReceivedWithoutDelta,
				runLength: 24,
			},
			wantBytes: []byte{0, 0x60, 0x18},
		},
	}
	for _, tt := range tests {
		tt := tt
		t1.Run(tt.name, func(t1 *testing.T) {
			t := &Responder{
				len: tt.fields.len,
			}
			t.writeRunLengthChunk(tt.args.symbol, tt.args.runLength)
			assert.Equal(t1, tt.wantBytes, t.payload[:t.len])
		})
	}
}

func TestTransportWideCC_writeStatusSymbolChunk(t1 *testing.T) {
	type fields struct {
		len uint16
	}
	type args struct {
		symbolSize uint16
		symbolList []uint16
	}
	tests := []struct {
		name      string
		fields    fields
		args      args
		wantBytes []byte
	}{
		{
			name: "Must not return error",
			args: args{
				symbolSize: rtcp.TypeTCCSymbolSizeOneBit,
				symbolList: []uint16{rtcp.TypeTCCPacketNotReceived,
					rtcp.TypeTCCPacketReceivedSmallDelta,
					rtcp.TypeTCCPacketReceivedSmallDelta,
					rtcp.TypeTCCPacketReceivedSmallDelta,
					rtcp.TypeTCCPacketReceivedSmallDelta,
					rtcp.TypeTCCPacketReceivedSmallDelta,
					rtcp.TypeTCCPacketNotReceived,
					rtcp.TypeTCCPacketNotReceived,
					rtcp.TypeTCCPacketNotReceived,
					rtcp.TypeTCCPacketReceivedSmallDelta,
					rtcp.TypeTCCPacketReceivedSmallDelta,
					rtcp.TypeTCCPacketReceivedSmallDelta,
					rtcp.TypeTCCPacketNotReceived,
					rtcp.TypeTCCPacketNotReceived},
			},
			wantBytes: []byte{0x9F, 0x1C},
		},
		{
			name: "Must set symbol chunk after padding",
			fields: fields{
				len: 1,
			},
			args: args{
				symbolSize: rtcp.TypeTCCSymbolSizeTwoBit,
				symbolList: []uint16{
					rtcp.TypeTCCPacketNotReceived,
					rtcp.TypeTCCPacketReceivedWithoutDelta,
					rtcp.TypeTCCPacketReceivedSmallDelta,
					rtcp.TypeTCCPacketReceivedSmallDelta,
					rtcp.TypeTCCPacketReceivedSmallDelta,
					rtcp.TypeTCCPacketNotReceived,
					rtcp.TypeTCCPacketNotReceived},
			},
			wantBytes: []byte{0x0, 0xcd, 0x50},
		},
	}
	for _, tt := range tests {
		tt := tt
		t1.Run(tt.name, func(t1 *testing.T) {
			t := &Responder{
				len: tt.fields.len,
			}
			for i, v := range tt.args.symbolList {
				t.createStatusSymbolChunk(tt.args.symbolSize, v, i)
			}
			t.writeStatusSymbolChunk(tt.args.symbolSize)
			assert.Equal(t1, tt.wantBytes, t.payload[:t.len])
		})
	}
}

func TestTransportWideCC_writeDelta(t1 *testing.T) {
	a := -32768
	type fields struct {
		deltaLen uint16
	}
	type args struct {
		deltaType uint16
		delta     uint16
	}
	tests := []struct {
		name   string
		fields fields
		args   args
		want   []byte
	}{
		{
			name: "Must set correct small delta",
			args: args{
				deltaType: rtcp.TypeTCCPacketReceivedSmallDelta,
				delta:     255,
			},
			want: []byte{0xff},
		},
		{
			name: "Must set correct small delta with padding",
			fields: fields{
				deltaLen: 1,
			},
			args: args{
				deltaType: rtcp.TypeTCCPacketReceivedSmallDelta,
				delta:     255,
			},
			want: []byte{0, 0xff},
		},
		{
			name: "Must set correct large delta",
			args: args{
				deltaType: rtcp.TypeTCCPacketReceivedLargeDelta,
				delta:     32767,
			},
			want: []byte{0x7F, 0xFF},
		},
		{
			name: "Must set correct large delta with padding",
			fields: fields{
				deltaLen: 1,
			},
			args: args{
				deltaType: rtcp.TypeTCCPacketReceivedLargeDelta,
				delta:     uint16(a),
			},
			want: []byte{0, 0x80, 0x00},
		},
	}
	for _, tt := range tests {
		tt := tt
		t1.Run(tt.name, func(t1 *testing.T) {
			t := &Responder{
				deltaLen: tt.fields.deltaLen,
			}
			t.writeDelta(tt.args.deltaType, tt.args.delta)
			assert.Equal(t1, tt.want, t.deltas[:t.deltaLen])
			assert.Equal(t1, tt.fields.deltaLen+tt.args.deltaType, t.deltaLen)
		})
	}
}

func TestTransportWideCC_writeHeader(t1 *testing.T) {
	type fields struct {
		tccPktCtn uint8
		sSSRC     uint32
		mSSRC     uint32
	}
	type args struct {
		bSN         uint16
		packetCount uint16
		refTime     uint32
	}
	tests := []struct {
		name   string
		fields fields
		args   args
		want   []byte
	}{
		{
			name: "Must construct correct header",
			fields: fields{
				tccPktCtn: 23,
				sSSRC:     4195875351,
				mSSRC:     1124282272,
			},
			args: args{
				bSN:         153,
				packetCount: 1,
				refTime:     4057090,
			},
			want: []byte{
				0xfa, 0x17, 0xfa, 0x17,
				0x43, 0x3, 0x2f, 0xa0,
				0x0, 0x99, 0x0, 0x1,
				0x3d, 0xe8, 0x2, 0x17},
		},
	}
	for _, tt := range tests {
		tt := tt
		t1.Run(tt.name, func(t1 *testing.T) {
			t := &Responder{
				pktCtn: tt.fields.tccPktCtn,
				sSSRC:  tt.fields.sSSRC,
				mSSRC:  tt.fields.mSSRC,
			}
			t.writeHeader(tt.args.bSN, tt.args.packetCount, tt.args.refTime)
			assert.Equal(t1, tt.want, t.payload[0:16])
		})
	}
}

func TestTccPacket(t1 *testing.T) {
	want := []byte{
		0xfa, 0x17, 0xfa, 0x17,
		0x43, 0x3, 0x2f, 0xa0,
		0x0, 0x99, 0x0, 0x1,
		0x3d, 0xe8, 0x2, 0x17,
		0x60, 0x18, 0x0, 0xdd,
		0x9F, 0x1C, 0xcd, 0x50,
	}

	delta := []byte{
		0xff, 0x80, 0xaa,
	}

	symbol1 := []uint16{rtcp.TypeTCCPacketNotReceived,
		rtcp.TypeTCCPacketReceivedSmallDelta,
		rtcp.TypeTCCPacketReceivedSmallDelta,
		rtcp.TypeTCCPacketReceivedSmallDelta,
		rtcp.TypeTCCPacketReceivedSmallDelta,
		rtcp.TypeTCCPacketReceivedSmallDelta,
		rtcp.TypeTCCPacketNotReceived,
		rtcp.TypeTCCPacketNotReceived,
		rtcp.TypeTCCPacketNotReceived,
		rtcp.TypeTCCPacketReceivedSmallDelta,
		rtcp.TypeTCCPacketReceivedSmallDelta,
		rtcp.TypeTCCPacketReceivedSmallDelta,
		rtcp.TypeTCCPacketNotReceived,
		rtcp.TypeTCCPacketNotReceived}
	symbol2 := []uint16{
		rtcp.TypeTCCPacketNotReceived,
		rtcp.TypeTCCPacketReceivedWithoutDelta,
		rtcp.TypeTCCPacketReceivedSmallDelta,
		rtcp.TypeTCCPacketReceivedSmallDelta,
		rtcp.TypeTCCPacketReceivedSmallDelta,
		rtcp.TypeTCCPacketNotReceived,
		rtcp.TypeTCCPacketNotReceived}

	t := &Responder{
		pktCtn: 23,
		sSSRC:  4195875351,
		mSSRC:  1124282272,
	}
	t.writeHeader(153, 1, 4057090)
	t.writeRunLengthChunk(rtcp.TypeTCCPacketReceivedWithoutDelta, 24)
	t.writeRunLengthChunk(rtcp.TypeTCCPacketNotReceived, 221)
	for i, v := range symbol1 {
		t.createStatusSymbolChunk(rtcp.TypeTCCSymbolSizeOneBit, v, i)
	}
	t.writeStatusSymbolChunk(rtcp.TypeTCCSymbolSizeOneBit)
	for i, v := range symbol2 {
		t.createStatusSymbolChunk(rtcp.TypeTCCSymbolSizeTwoBit, v, i)
	}
	t.writeStatusSymbolChunk(rtcp.TypeTCCSymbolSizeTwoBit)
	t.deltaLen = uint16(len(delta))
	assert.Equal(t1, want, t.payload[:24])

	pLen := t.len + t.deltaLen + 4
	pad := pLen%4 != 0
	for pLen%4 != 0 {
		pLen++
	}
	hdr := rtcp.Header{
		Padding: pad,
		Length:  (pLen / 4) - 1,
		Count:   rtcp.FormatTCC,
		Type:    rtcp.TypeTransportSpecificFeedback,
	}
	assert.Equal(t1, int(pLen), len(want)+3+4+1)
	hb, _ := hdr.Marshal()
	pkt := make([]byte, pLen)
	copy(pkt, hb)
	assert.Equal(t1, hb, pkt[:len(hb)])
	copy(pkt[4:], t.payload[:t.len])
	assert.Equal(t1, append(hb, t.payload[:t.len]...), pkt[:len(hb)+int(t.len)])
	copy(pkt[4+t.len:], delta[:t.deltaLen])
	assert.Equal(t1, delta, pkt[len(hb)+int(t.len):len(pkt)-1])
	var ss rtcp.TransportLayerCC
	err := ss.Unmarshal(pkt)
	assert.NoError(t1, err)

	assert.Equal(t1, hdr, ss.Header)

}

func BenchmarkBuildPacket(b *testing.B) {
	rand.Seed(time.Now().UnixNano())
	b.ReportAllocs()
	n := 1 + rand.Intn(4-1+1)
	var twcc Responder
	tm := time.Now()
	for i := 1; i < 100; i++ {
		tm := tm.Add(time.Duration(60*n) * time.Millisecond)
		twcc.extInfo = append(twcc.extInfo, rtpExtInfo{
			ExtTSN:    uint32(i),
			Timestamp: tm.UnixNano(),
		})
	}
	for i := 0; i < b.N; i++ {
		_ = twcc.buildTransportCCPacket()
	}
}
