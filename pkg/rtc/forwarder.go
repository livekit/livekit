package rtc

import (
	"context"
	"io"
	"sync"
	"time"

	"github.com/livekit/livekit-server/pkg/logger"
	"github.com/livekit/livekit-server/pkg/sfu"
	"github.com/pion/ion-sfu/pkg/log"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
)

// a forwarder publishes data to a target track or datachannel
// manages the RTCP loop with the target peer
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
	ctx         context.Context
	cancel      context.CancelFunc
	rtcpWriter  RTCPWriter        // write RTCP to source peer
	sender      *webrtc.RTPSender // destination sender
	track       *webrtc.Track     // sender track
	buffer      *sfu.Buffer
	channelType ChannelType
	payload     uint8

	lastPli   time.Time
	createdAt time.Time

	once sync.Once

	// handlers
	onClose func(forwarder Forwarder)
}

func NewSimpleForwarder(ctx context.Context, rtcpWriter RTCPWriter, sender *webrtc.RTPSender, buffer *sfu.Buffer) *SimpleForwarder {
	ctx, cancel := context.WithCancel(ctx)
	f := &SimpleForwarder{
		ctx:        ctx,
		cancel:     cancel,
		rtcpWriter: rtcpWriter,
		sender:     sender,
		buffer:     buffer,
		track:      sender.Track(),
		payload:    sender.Track().PayloadType(),
		createdAt:  time.Now(),
	}

	if f.track.Kind() == webrtc.RTPCodecTypeAudio {
		f.channelType = ChannelAudio
	} else if f.track.Kind() == webrtc.RTPCodecTypeVideo {
		f.channelType = ChannelVideo
	}

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
	if f.ctx.Err() != nil {
		return
	}
	f.cancel()
	if f.onClose != nil {
		f.onClose(f)
	}
}

// Writes an RTP packet to peer
func (f *SimpleForwarder) WriteRTP(pkt *rtp.Packet) error {
	if f.ctx.Err() != nil {
		// skip canceled context errors
		return nil
	}

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
		pkts, err := f.sender.ReadRTCP()
		if err == io.ErrClosedPipe {
			f.Close()
			return
		}

		if f.ctx.Err() != nil {
			return
		}

		if err != nil {
			// TODO: log error
		}

		var fwdPkts []rtcp.Packet
		for _, pkt := range pkts {
			switch pkt := pkt.(type) {
			case *rtcp.PictureLossIndication, *rtcp.FullIntraRequest:
				fwdPkts = append(fwdPkts, pkt)
				f.lastPli = time.Now()
			case *rtcp.TransportLayerNack:
				log.Tracef("sender got nack: %+v", pkt)
				for _, pair := range pkt.Nacks {
					if err := f.buffer.WritePacket(
						pair.PacketID,
						f.track,
						0,
						0,
						f.track.SSRC(),
					); err == sfu.ErrPacketNotFound {
						// TODO handle missing nacks in sfu cache
					}
				}
			default:
				// TODO: Use fb packets for congestion control
			}
		}

		if len(fwdPkts) > 0 {
			if err := f.rtcpWriter.WriteRTCP(fwdPkts); err != nil {
				logger.GetLogger().Warnw("could not forward RTCP to peer",
					"err", err)
			}
		}
	}
}
