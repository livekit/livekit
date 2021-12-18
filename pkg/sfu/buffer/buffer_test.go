package buffer

import (
	"sync"
	"testing"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
	"github.com/stretchr/testify/assert"
)

var vp8Codec = webrtc.RTPCodecParameters{
	RTPCodecCapability: webrtc.RTPCodecCapability{
		MimeType:  "video/vp8",
		ClockRate: 90000,
		RTCPFeedback: []webrtc.RTCPFeedback{{
			Type: "nack",
		}},
	},
	PayloadType: 96,
}

var opusCodec = webrtc.RTPCodecParameters{
	RTPCodecCapability: webrtc.RTPCodecCapability{
		MimeType:  "audio/opus",
		ClockRate: 48000,
	},
	PayloadType: 96,
}

func TestNack(t *testing.T) {
	pool := &sync.Pool{
		New: func() interface{} {
			b := make([]byte, 1500)
			return &b
		},
	}
	logger := Logger

	t.Run("nack normal", func(t *testing.T) {
		buff := NewBuffer(123, pool, pool, logger)
		buff.codecType = webrtc.RTPCodecTypeVideo
		assert.NotNil(t, buff)
		var wg sync.WaitGroup
		// 3 nacks 1 Pli
		wg.Add(4)
		buff.OnFeedback(func(fb []rtcp.Packet) {
			for _, pkt := range fb {
				switch p := pkt.(type) {
				case *rtcp.TransportLayerNack:
					if p.Nacks[0].PacketList()[0] == 1 && p.MediaSSRC == 123 {
						wg.Done()
					}
				case *rtcp.PictureLossIndication:
					if p.MediaSSRC == 123 {
						wg.Done()
					}
				}
			}
		})
		buff.Bind(webrtc.RTPParameters{
			HeaderExtensions: nil,
			Codecs:           []webrtc.RTPCodecParameters{vp8Codec},
		}, vp8Codec.RTPCodecCapability, Options{})
		for i := 0; i < 15; i++ {
			if i == 1 {
				continue
			}
			pkt := rtp.Packet{
				Header:  rtp.Header{SequenceNumber: uint16(i), Timestamp: uint32(i)},
				Payload: []byte{0xff, 0xff, 0xff, 0xfd, 0xb4, 0x9f, 0x94, 0x1},
			}
			b, err := pkt.Marshal()
			assert.NoError(t, err)
			_, err = buff.Write(b)
			assert.NoError(t, err)
		}
		wg.Wait()

	})

	t.Run("nack with seq wrap", func(t *testing.T) {
		buff := NewBuffer(123, pool, pool, logger)
		buff.codecType = webrtc.RTPCodecTypeVideo
		assert.NotNil(t, buff)
		var wg sync.WaitGroup
		expects := map[uint16]int{
			65534: 0,
			65535: 0,
			0:     0,
			1:     0,
		}
		wg.Add(3 * len(expects)) // retry 3 times
		buff.OnFeedback(func(fb []rtcp.Packet) {
			for _, pkt := range fb {
				switch p := pkt.(type) {
				case *rtcp.TransportLayerNack:
					if p.MediaSSRC == 123 {
						for _, v := range p.Nacks {
							v.Range(func(seq uint16) bool {
								if _, ok := expects[seq]; ok {
									wg.Done()
								} else {
									assert.Fail(t, "unexpected nack seq ", seq)
								}
								return true
							})
						}
					}
				case *rtcp.PictureLossIndication:
					if p.MediaSSRC == 123 {
						// wg.Done()
					}
				}
			}
		})
		buff.Bind(webrtc.RTPParameters{
			HeaderExtensions: nil,
			Codecs:           []webrtc.RTPCodecParameters{vp8Codec},
		}, vp8Codec.RTPCodecCapability, Options{})
		for i := 0; i < 15; i++ {
			if i > 0 && i < 5 {
				continue
			}
			pkt := rtp.Packet{
				Header:  rtp.Header{SequenceNumber: uint16(i + 65533), Timestamp: uint32(i)},
				Payload: []byte{0xff, 0xff, 0xff, 0xfd, 0xb4, 0x9f, 0x94, 0x1},
			}
			b, err := pkt.Marshal()
			assert.NoError(t, err)
			_, err = buff.Write(b)
			assert.NoError(t, err)
		}
		wg.Wait()

	})
}

func TestNewBuffer(t *testing.T) {
	type args struct {
		options Options
		ssrc    uint32
	}
	tests := []struct {
		name string
		args args
	}{
		{
			name: "Must not be nil and add packets in sequence",
			args: args{
				options: Options{
					MaxBitRate: 1e6,
				},
			},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
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
				{
					Header: rtp.Header{
						SequenceNumber: 65535,
					},
				},
			}
			pool := &sync.Pool{
				New: func() interface{} {
					b := make([]byte, 1500)
					return &b
				},
			}
			logger := Logger
			buff := NewBuffer(123, pool, pool, logger)
			buff.codecType = webrtc.RTPCodecTypeVideo
			assert.NotNil(t, buff)
			assert.NotNil(t, TestPackets)
			buff.OnFeedback(func(_ []rtcp.Packet) {
			})
			buff.Bind(webrtc.RTPParameters{
				HeaderExtensions: nil,
				Codecs:           []webrtc.RTPCodecParameters{vp8Codec},
			}, vp8Codec.RTPCodecCapability, Options{})

			for _, p := range TestPackets {
				buf, _ := p.Marshal()
				_, _ = buff.Write(buf)
			}
			// assert.Equal(t, 6, buff.PacketQueue.size)
			assert.Equal(t, uint32(1<<16), buff.seqHdlr.Cycles())
			assert.Equal(t, uint16(2), uint16(buff.seqHdlr.MaxSeqNo()))
		})
	}
}

func TestFractionLostReport(t *testing.T) {
	pool := &sync.Pool{
		New: func() interface{} {
			b := make([]byte, 1500)
			return &b
		},
	}
	buff := NewBuffer(123, pool, pool, Logger)
	buff.codecType = webrtc.RTPCodecTypeVideo
	assert.NotNil(t, buff)
	var wg sync.WaitGroup
	wg.Add(1)
	buff.SetLastFractionLostReport(55)
	buff.OnFeedback(func(fb []rtcp.Packet) {
		for _, pkt := range fb {
			switch p := pkt.(type) {
			case *rtcp.ReceiverReport:
				for _, v := range p.Reports {
					assert.EqualValues(t, 55, v.FractionLost)
				}
				wg.Done()
			}
		}
	})
	buff.Bind(webrtc.RTPParameters{
		HeaderExtensions: nil,
		Codecs:           []webrtc.RTPCodecParameters{opusCodec},
	}, opusCodec.RTPCodecCapability, Options{})
	for i := 0; i < 15; i++ {
		pkt := rtp.Packet{
			Header:  rtp.Header{SequenceNumber: uint16(i), Timestamp: uint32(i)},
			Payload: []byte{0xff, 0xff, 0xff, 0xfd, 0xb4, 0x9f, 0x94, 0x1},
		}
		b, err := pkt.Marshal()
		assert.NoError(t, err)
		if i == 1 {
			time.Sleep(1 * time.Second)
		}
		_, err = buff.Write(b)
		assert.NoError(t, err)
	}
	wg.Wait()
}

func TestSeqWrapHandler(t *testing.T) {
	s := SeqWrapHandler{}
	s.UpdateMaxSeq(1)
	assert.Equal(t, uint32(1), s.MaxSeqNo())

	type caseInfo struct {
		seqs  []uint32 // {seq1, seq2, unwrap of seq2}
		newer bool     // seq2 is newer than seq1
	}
	// test normal case, name -> {seq1, seq2, unwrap of seq2}
	cases := map[string]caseInfo{
		"no wrap":                             {[]uint32{1, 4, 4}, true},
		"no wrap backward":                    {[]uint32{4, 1, 1}, false},
		"wrap around forward to zero":         {[]uint32{65534, 0, 65536}, true},
		"wrap around forward":                 {[]uint32{65534, 10, 65546}, true},
		"wrap around forward 2":               {[]uint32{65535 + 65536*2, 1, 1 + 65536*3}, true},
		"wrap around backward ":               {[]uint32{5, 65534, 65534}, false},
		"wrap around backward less than zero": {[]uint32{5, 65534, 65534}, false},
	}

	for k, v := range cases {
		t.Run(k, func(t *testing.T) {
			s := SeqWrapHandler{}
			s.UpdateMaxSeq(v.seqs[0])
			extsn, newer := s.Unwrap(uint16(v.seqs[1]))
			assert.Equal(t, v.newer, newer)
			assert.Equal(t, v.seqs[2], extsn)
		})
	}

}

func TestIsTimestampWrap(t *testing.T) {
	type caseInfo struct {
		name  string
		ts1   uint32
		ts2   uint32
		later bool
	}

	cases := []caseInfo{
		{"normal case 1 timestamp later ", 2, 1, true},
		{"normal case 2 timestamp later", 0x1c000000, 0x10000000, true},
		{"wrap case timestamp later", 0xffff, 0xfc000000, true},
		{"wrap case timestamp early", 0xfc000000, 0xffff, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.later, IsLaterTimestamp(c.ts1, c.ts2))
		})
	}
}
