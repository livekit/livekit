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

package buffer

import (
	"math"
	"sync"
	"testing"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"github.com/stretchr/testify/require"

	"github.com/livekit/mediatransportutil/pkg/nack"
)

var h265Codec = webrtc.RTPCodecParameters{
	RTPCodecCapability: webrtc.RTPCodecCapability{
		MimeType:  "video/h265",
		ClockRate: 90000,
		RTCPFeedback: []webrtc.RTCPFeedback{{
			Type: "nack",
		}},
	},
	PayloadType: 116,
}

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
	PayloadType: 111,
}

func TestNack(t *testing.T) {
	t.Run("nack normal", func(t *testing.T) {
		buff := NewBuffer(123, 1, 1)
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
		}, vp8Codec.RTPCodecCapability, 0)
		rtt := uint32(20)
		buff.nacker.SetRTT(rtt)
		for i := 0; i < 15; i++ {
			if i == 1 {
				continue
			}
			if i < 14 {
				time.Sleep(time.Duration(float64(rtt)*math.Pow(nack.NackQueueParamsDefault.BackoffFactor, float64(i))+10) * time.Millisecond)
			} else {
				time.Sleep(500 * time.Millisecond) // even a long wait should not exceed max retries
			}
			pkt := rtp.Packet{
				Header: rtp.Header{
					Version:        2,
					PayloadType:    96,
					SequenceNumber: uint16(i),
					Timestamp:      uint32(i),
					SSRC:           123,
				},
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
		buff := NewBuffer(123, 1, 1)
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
		}, vp8Codec.RTPCodecCapability, 0)
		rtt := uint32(30)
		buff.nacker.SetRTT(rtt)
		for i := 0; i < 15; i++ {
			if i > 0 && i < 5 {
				continue
			}
			if i < 14 {
				time.Sleep(time.Duration(float64(rtt)*math.Pow(nack.NackQueueParamsDefault.BackoffFactor, float64(i))+10) * time.Millisecond)
			} else {
				time.Sleep(500 * time.Millisecond) // even a long wait should not exceed max retries
			}
			pkt := rtp.Packet{
				Header: rtp.Header{
					Version:        2,
					PayloadType:    96,
					SequenceNumber: uint16(i + 65533),
					Timestamp:      uint32(i),
					SSRC:           123,
				},
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
	tests := []struct {
		name string
	}{
		{
			name: "Must not be nil and add packets in sequence",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var TestPackets = []*rtp.Packet{
				{
					Header: rtp.Header{
						Version:        2,
						PayloadType:    96,
						SequenceNumber: 65533,
						SSRC:           123,
					},
				},
				{
					Header: rtp.Header{
						Version:        2,
						PayloadType:    96,
						SequenceNumber: 65534,
						SSRC:           123,
					},
					Payload: []byte{1},
				},
				{
					Header: rtp.Header{
						Version:        2,
						PayloadType:    96,
						SequenceNumber: 2,
						SSRC:           123,
					},
				},
				{
					Header: rtp.Header{
						Version:        2,
						PayloadType:    96,
						SequenceNumber: 65535,
						SSRC:           123,
					},
				},
			}
			buff := NewBuffer(123, 1, 1)
			buff.codecType = webrtc.RTPCodecTypeVideo
			require.NotNil(t, buff)
			buff.OnRtcpFeedback(func(_ []rtcp.Packet) {})
			buff.Bind(webrtc.RTPParameters{
				HeaderExtensions: nil,
				Codecs:           []webrtc.RTPCodecParameters{vp8Codec},
			}, vp8Codec.RTPCodecCapability, 0)

			for _, p := range TestPackets {
				buf, _ := p.Marshal()
				_, _ = buff.Write(buf)
			}
			require.Equal(t, uint16(2), buff.rtpStats.HighestSequenceNumber())
			require.Equal(t, uint64(65536+2), buff.rtpStats.ExtendedHighestSequenceNumber())
		})
	}
}

func TestFractionLostReport(t *testing.T) {
	buff := NewBuffer(123, 1, 1)
	require.NotNil(t, buff)

	var wg sync.WaitGroup

	// with loss proxying
	wg.Add(1)
	buff.SetAudioLossProxying(true)
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
	}, opusCodec.RTPCodecCapability, 0)
	for i := 0; i < 15; i++ {
		pkt := rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				PayloadType:    111,
				SequenceNumber: uint16(i),
				Timestamp:      uint32(i),
				SSRC:           123,
			},
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

	wg.Add(1)
	buff.SetAudioLossProxying(false)
	buff.OnRtcpFeedback(func(fb []rtcp.Packet) {
		for _, pkt := range fb {
			switch p := pkt.(type) {
			case *rtcp.ReceiverReport:
				for _, v := range p.Reports {
					require.EqualValues(t, 0, v.FractionLost)
				}
				wg.Done()
			}
		}
	})
	buff.Bind(webrtc.RTPParameters{
		HeaderExtensions: nil,
		Codecs:           []webrtc.RTPCodecParameters{opusCodec},
	}, opusCodec.RTPCodecCapability, 0)
	for i := 0; i < 15; i++ {
		pkt := rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				PayloadType:    111,
				SequenceNumber: uint16(i),
				Timestamp:      uint32(i),
				SSRC:           123,
			},
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

func TestCodecChange(t *testing.T) {
	// codec change before bind
	buff := NewBuffer(123, 1, 1)
	require.NotNil(t, buff)
	changedCodec := make(chan webrtc.RTPCodecParameters, 1)
	buff.OnCodecChange(func(rp webrtc.RTPCodecParameters) {
		select {
		case changedCodec <- rp:
		default:
			t.Fatalf("codec change not consumed")
		}
	})

	h265Pkt := rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    116,
			SequenceNumber: 1,
			Timestamp:      1,
			SSRC:           123,
		},
		Payload: []byte{0xff, 0xff, 0xff, 0xfd, 0xb4, 0x9f, 0x94, 0x1},
	}
	buf, err := h265Pkt.Marshal()
	require.NoError(t, err)
	_, err = buff.Write(buf)
	require.NoError(t, err)

	select {
	case <-changedCodec:
		t.Fatalf("unexpected codec change")
	case <-time.After(100 * time.Millisecond):
	}

	buff.Bind(webrtc.RTPParameters{
		HeaderExtensions: nil,
		Codecs:           []webrtc.RTPCodecParameters{vp8Codec, h265Codec},
	}, vp8Codec.RTPCodecCapability, 0)

	select {
	case c := <-changedCodec:
		require.Equal(t, h265Codec, c)

	case <-time.After(1 * time.Second):
		t.Fatalf("expected codec change")
	}

	// codec change after bind
	vp8Pkt := rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    96,
			SequenceNumber: 3,
			Timestamp:      3,
			SSRC:           123,
		},
		Payload: []byte{0xff, 0xff, 0xff, 0xfd, 0xb4, 0x9f, 0x94, 0x1},
	}
	buf, err = vp8Pkt.Marshal()
	require.NoError(t, err)
	_, err = buff.Write(buf)
	require.NoError(t, err)

	select {
	case c := <-changedCodec:
		require.Equal(t, vp8Codec, c)

	case <-time.After(1 * time.Second):
		t.Fatalf("expected codec change")
	}

	// out of order pkts can't cause codec change
	h265Pkt.SequenceNumber = 2
	h265Pkt.Timestamp = 2
	buf, err = h265Pkt.Marshal()
	require.NoError(t, err)
	_, err = buff.Write(buf)
	require.NoError(t, err)
	select {
	case <-changedCodec:
		t.Fatalf("unexpected codec change")
	case <-time.After(100 * time.Millisecond):
	}

	// unknown codec should not cause change
	h265Pkt.SequenceNumber = 4
	h265Pkt.Timestamp = 4
	h265Pkt.PayloadType = 117
	buf, err = h265Pkt.Marshal()
	require.NoError(t, err)
	_, err = buff.Write(buf)
	require.NoError(t, err)
	select {
	case <-changedCodec:
		t.Fatalf("unexpected codec change")
	case <-time.After(100 * time.Millisecond):
	}
}

func BenchmarkMemcpu(b *testing.B) {
	buf := make([]byte, 1500*1500*10)
	buf2 := make([]byte, 1500*1500*20)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		copy(buf2, buf)
	}
}
