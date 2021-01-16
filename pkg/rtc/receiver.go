package rtc

import (
	"github.com/pion/ion-sfu/pkg/buffer"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"

	"github.com/livekit/livekit-server/pkg/logger"
)

const (
	// TODO: could probably increase this depending on Configuration/memory
	maxChanSize = 1024
)

// A receiver is responsible for pulling from a single logical track
type ReceiverImpl struct {
	rtpReceiver *webrtc.RTPReceiver
	track       *webrtc.TrackRemote
	buffer      *buffer.Buffer
	rtcpReader  *buffer.RTCPReader
	rtcpChan    chan []rtcp.Packet
}

func NewReceiver(rtcpCh chan []rtcp.Packet, rtpReceiver *webrtc.RTPReceiver, track *webrtc.TrackRemote, config ReceiverConfig) *ReceiverImpl {
	r := &ReceiverImpl{
		rtpReceiver: rtpReceiver,
		rtcpChan:    rtcpCh,
		track:       track,
	}

	r.buffer, r.rtcpReader = bufferFactory.GetBufferPair(uint32(track.SSRC()))

	// when we have feedback for the sender, send through the rtcp channel
	r.buffer.OnFeedback(func(fb []rtcp.Packet) {
		if r.rtcpChan != nil {
			r.rtcpChan <- fb
		}
	})

	r.buffer.OnTransportWideCC(func(sn uint16, timeNS int64, marker bool) {
		// TODO: figure out how to handle this
	})

	r.buffer.Bind(rtpReceiver.GetParameters(), buffer.Options{
		BufferTime: config.maxBufferTime,
		MaxBitRate: config.maxBitrate,
	})

	// received sender updates
	r.rtcpReader.OnPacket(func(bytes []byte) {
		pkts, err := rtcp.Unmarshal(bytes)
		if err != nil {
			logger.Warnw("could not unmarshal RTCP packet")
			return
		}
		for _, pkt := range pkts {
			switch p := pkt.(type) {
			case *rtcp.SenderReport:
				r.buffer.SetSenderReportData(p.RTPTime, p.NTPTime)
			case *rtcp.SourceDescription:
				// TODO: see what we'd want to do with this
			}
		}
	})

	return r
}

// PacketBuffer interface, retrieves a packet from buffer and deserializes
// it's possible that the packet can't be found, or the connection has been closed (io.EOF)
func (r *ReceiverImpl) GetBufferedPacket(pktBuf []byte, sn uint16, snOffset uint16) (p rtp.Packet, err error) {
	if pktBuf == nil {
		pktBuf = make([]byte, rtpPacketMaxSize)
	}
	size, err := r.buffer.GetPacket(pktBuf, sn+snOffset)
	if err != nil {
		return
	}

	err = p.Unmarshal(pktBuf[:size])
	return
}

func (r *ReceiverImpl) RTPChan() <-chan rtp.Packet {
	return r.buffer.PacketChan()
}
