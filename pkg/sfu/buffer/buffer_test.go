package buffer

import (
	"math"
	"sync"
	"testing"
	"time"

	"github.com/livekit/mediatransportutil/pkg/nack"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
	"github.com/stretchr/testify/require"
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

	t.Run("nack normal", func(t *testing.T) {
		buff := NewBuffer(123, pool, pool)
		buff.codecType = webrtc.RTPCodecTypeVideo
		require.NotNil(t, buff)
		var wg sync.WaitGroup
		// 5 tries
		wg.Add(5)
		buff.OnRtcpFeedback(func(fb []rtcp.Packet) {
			for _, pkt := range fb {
				switch p := pkt.(type) {
				case *rtcp.TransportLayerNack:
					if p.Nacks[0].PacketList()[0] == 1 && p.MediaSSRC == 123 {
						wg.Done()
					}
				}
			}
		})
		buff.Bind(webrtc.RTPParameters{
			HeaderExtensions: nil,
			Codecs:           []webrtc.RTPCodecParameters{vp8Codec},
		}, vp8Codec.RTPCodecCapability)
		rtt := uint32(20)
		buff.nacker.SetRTT(rtt)
		for i := 0; i < 15; i++ {
			if i == 1 {
				continue
			}
			if i < 14 {
				time.Sleep(time.Duration(float64(rtt)*math.Pow(nack.BackoffFactor, float64(i))+10) * time.Millisecond)
			} else {
				time.Sleep(500 * time.Millisecond) // even a long wait should not exceed max retries
			}
			pkt := rtp.Packet{
				Header:  rtp.Header{SequenceNumber: uint16(i), Timestamp: uint32(i)},
				Payload: []byte{0xff, 0xff, 0xff, 0xfd, 0xb4, 0x9f, 0x94, 0x1},
			}
			b, err := pkt.Marshal()
			require.NoError(t, err)
			_, err = buff.Write(b)
			require.NoError(t, err)
		}
		wg.Wait()

	})

	t.Run("nack with seq wrap", func(t *testing.T) {
		buff := NewBuffer(123, pool, pool)
		buff.codecType = webrtc.RTPCodecTypeVideo
		require.NotNil(t, buff)
		var wg sync.WaitGroup
		expects := map[uint16]int{
			65534: 0,
			65535: 0,
			0:     0,
			1:     0,
		}
		wg.Add(5 * len(expects)) // retry 5 times
		buff.OnRtcpFeedback(func(fb []rtcp.Packet) {
			for _, pkt := range fb {
				switch p := pkt.(type) {
				case *rtcp.TransportLayerNack:
					if p.MediaSSRC == 123 {
						for _, v := range p.Nacks {
							v.Range(func(seq uint16) bool {
								if _, ok := expects[seq]; ok {
									wg.Done()
								} else {
									require.Fail(t, "unexpected nack seq ", seq)
								}
								return true
							})
						}
					}
				}
			}
		})
		buff.Bind(webrtc.RTPParameters{
			HeaderExtensions: nil,
			Codecs:           []webrtc.RTPCodecParameters{vp8Codec},
		}, vp8Codec.RTPCodecCapability)
		rtt := uint32(30)
		buff.nacker.SetRTT(rtt)
		for i := 0; i < 15; i++ {
			if i > 0 && i < 5 {
				continue
			}
			if i < 14 {
				time.Sleep(time.Duration(float64(rtt)*math.Pow(nack.BackoffFactor, float64(i))+10) * time.Millisecond)
			} else {
				time.Sleep(500 * time.Millisecond) // even a long wait should not exceed max retries
			}
			pkt := rtp.Packet{
				Header:  rtp.Header{SequenceNumber: uint16(i + 65533), Timestamp: uint32(i)},
				Payload: []byte{0xff, 0xff, 0xff, 0xfd, 0xb4, 0x9f, 0x94, 0x1},
			}
			b, err := pkt.Marshal()
			require.NoError(t, err)
			_, err = buff.Write(b)
			require.NoError(t, err)
		}
		wg.Wait()

	})
}

func TestNewBuffer(t *testing.T) {
	type args struct {
		options Options
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
			buff := NewBuffer(123, pool, pool)
			buff.codecType = webrtc.RTPCodecTypeVideo
			require.NotNil(t, buff)
			buff.OnRtcpFeedback(func(_ []rtcp.Packet) {
			})
			buff.Bind(webrtc.RTPParameters{
				HeaderExtensions: nil,
				Codecs:           []webrtc.RTPCodecParameters{vp8Codec},
			}, vp8Codec.RTPCodecCapability)

			for _, p := range TestPackets {
				buf, _ := p.Marshal()
				_, _ = buff.Write(buf)
			}
			require.Equal(t, uint16(1), buff.rtpStats.cycles)
			require.Equal(t, uint16(2), buff.rtpStats.highestSN)
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
	buff := NewBuffer(123, pool, pool)
	require.NotNil(t, buff)
	buff.codecType = webrtc.RTPCodecTypeVideo
	var wg sync.WaitGroup
	wg.Add(1)
	buff.SetLastFractionLostReport(55)
	buff.OnRtcpFeedback(func(fb []rtcp.Packet) {
		for _, pkt := range fb {
			switch p := pkt.(type) {
			case *rtcp.ReceiverReport:
				for _, v := range p.Reports {
					require.EqualValues(t, 55, v.FractionLost)
				}
				wg.Done()
			}
		}
	})
	buff.Bind(webrtc.RTPParameters{
		HeaderExtensions: nil,
		Codecs:           []webrtc.RTPCodecParameters{opusCodec},
	}, opusCodec.RTPCodecCapability)
	for i := 0; i < 15; i++ {
		pkt := rtp.Packet{
			Header:  rtp.Header{SequenceNumber: uint16(i), Timestamp: uint32(i)},
			Payload: []byte{0xff, 0xff, 0xff, 0xfd, 0xb4, 0x9f, 0x94, 0x1},
		}
		b, err := pkt.Marshal()
		require.NoError(t, err)
		if i == 1 {
			time.Sleep(1 * time.Second)
		}
		_, err = buff.Write(b)
		require.NoError(t, err)
	}
	wg.Wait()
}
