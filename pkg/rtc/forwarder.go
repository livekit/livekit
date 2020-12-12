package rtc

import (
	"context"
	"io"
	"sync"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"

	"github.com/livekit/livekit-server/pkg/logger"
	"github.com/livekit/livekit-server/pkg/sfu"
)

type PacketBuffer interface {
	GetBufferedPackets(mediaSSRC uint32, snOffset uint16, tsOffset uint32, sn []uint16) []rtp.Packet
}

// a forwarder publishes data to a target remoteTrack or datachannel
// manages the RTCP loop with the target participant
type Forwarder interface {
	ChannelType() ChannelType
	WriteRTP(*rtp.Packet) error
	Start()
	Close()
	CreatedAt() time.Time

	OnClose(func(Forwarder))
}

type RTCPWriter interface {
	WriteRTCP(pkts []rtcp.Packet) error
}

type ChannelType int

const (
	ChannelAudio ChannelType = iota + 1
	ChannelVideo
	ChannelData
)

type SimpleForwarder struct {
	ctx          context.Context
	cancel       context.CancelFunc
	sourceRtcpCh chan []rtcp.Packet // channel to write RTCP packets to source
	track        *sfu.DownTrack     // sender track
	packetBuffer PacketBuffer
	channelType  ChannelType

	lastPli   time.Time
	createdAt time.Time

	once sync.Once

	// handlers
	onClose func(forwarder Forwarder)
}

func NewSimpleForwarder(rtcpCh chan []rtcp.Packet, track *sfu.DownTrack, pb PacketBuffer) *SimpleForwarder {
	ctx, cancel := context.WithCancel(context.Background())
	f := &SimpleForwarder{
		ctx:          ctx,
		cancel:       cancel,
		sourceRtcpCh: rtcpCh,
		packetBuffer: pb,
		track:        track,
		createdAt:    time.Now(),
	}

	if f.track.Kind() == webrtc.RTPCodecTypeAudio {
		f.channelType = ChannelAudio
	} else if f.track.Kind() == webrtc.RTPCodecTypeVideo {
		f.channelType = ChannelVideo
	}

	// when underlying track is closed, close the forwarder too
	track.OnCloseHandler(f.Close)

	return f
}

func (f *SimpleForwarder) ChannelType() ChannelType {
	return f.channelType
}

func (f *SimpleForwarder) Start() {
	f.once.Do(func() {
		go f.rtcpWorker()
	})
}

func (f *SimpleForwarder) Close() {
	//if f.ctx.Err() != nil {
	//	return
	//}
	//f.cancel()
	if f.onClose != nil {
		f.onClose(f)
	}
}

// Writes an RTP packet to peer
func (f *SimpleForwarder) WriteRTP(pkt *rtp.Packet) error {
	//if f.ctx.Err() != nil {
	//	// skip canceled context errors
	//	return nil
	//}

	err := f.track.WriteRTP(pkt)

	if err != nil {
		if err == io.ErrClosedPipe {
			// TODO: log and error? how should this error be handled
			return nil
		}
		return err
	}
	return nil
}

func (f *SimpleForwarder) OnClose(closeFunc func(Forwarder)) {
	f.onClose = closeFunc
}

func (f *SimpleForwarder) CreatedAt() time.Time {
	return f.createdAt
}

// rtcpWorker receives RTCP packets from the destination peer
// this include packet loss packets
func (f *SimpleForwarder) rtcpWorker() {
	for {
		pkts, err := f.track.RTPSender().ReadRTCP()
		if err == io.ErrClosedPipe {
			f.Close()
			return
		}

		//if f.ctx.Err() != nil {
		//	return
		//}

		if err != nil {
			logger.GetLogger().Errorw("could not write read RTCP",
				"err", err)
		}

		var fwdPkts []rtcp.Packet
		pliOnce := true
		firOnce := true
		for _, pkt := range pkts {
			switch p := pkt.(type) {
			case *rtcp.PictureLossIndication:
				if pliOnce {
					p.MediaSSRC = f.track.LastSSRC()
					p.SenderSSRC = f.track.LastSSRC()
					fwdPkts = append(fwdPkts, p)
					pliOnce = false
				}
			case *rtcp.FullIntraRequest:
				if firOnce {
					p.MediaSSRC = f.track.LastSSRC()
					p.SenderSSRC = f.track.SSRC()
					fwdPkts = append(fwdPkts, p)
					firOnce = false
				}
			case *rtcp.ReceiverReport:
				if len(p.Reports) > 0 && p.Reports[0].FractionLost > 25 {
					//log.Tracef("Slow link for sender %s, fraction packet lost %.2f", f.track.peerID, float64(p.Reports[0].FractionLost)/256)
				}
			case *rtcp.TransportLayerNack:
				logger.GetLogger().Debugw("forwarder got nack",
					"packet", p)
				for _, pair := range p.Nacks {
					bufferedPackets := f.packetBuffer.GetBufferedPackets(
						f.track.SSRC(),
						f.track.SnOffset(),
						f.track.TsOffset(),
						f.track.GetNACKSeqNo(pair.PacketList()),
					)
					for i, _ := range bufferedPackets {
						pt := bufferedPackets[i]
						f.track.WriteRTP(&rtp.Packet{Header: pt.Header, Payload: pt.Payload})
					}
				}
			default:
				// TODO: Use fb packets for congestion control
			}
		}

		if len(fwdPkts) > 0 {
			f.sourceRtcpCh <- fwdPkts
		}
	}
}
