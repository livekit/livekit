package rtc

import (
	"context"
	"io"
	"sync"
	"time"

	"github.com/livekit/livekit-server/pkg/sfu"
	"github.com/pion/ion-sfu/pkg/log"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
)

// a forwarder publishes data to a target track or datachannel
// manages the RTCP loop with the target peer
type Forwarder interface {
	ID() string
	ChannelType() ChannelType
	WriteRTP(*rtp.Packet) error
	Start()
	Close()
}

type ChannelType int

const (
	ChannelAudio ChannelType = iota + 1
	ChannelVideo
	ChannelData
)

type SimpleForwarder struct {
	id          string
	ctx         context.Context
	cancel      context.CancelFunc
	peer        *WebRTCPeer       // source peer
	sender      *webrtc.RTPSender // destination sender
	track       *webrtc.Track     // sender track
	buffer      *sfu.Buffer
	channelType ChannelType
	payload     uint8

	lastPli time.Time

	once sync.Once
}

func NewSimpleForwarder(ctx context.Context, peer *WebRTCPeer, sender *webrtc.RTPSender, buffer *sfu.Buffer) *SimpleForwarder {
	ctx, cancel := context.WithCancel(ctx)
	f := &SimpleForwarder{
		id:      peer.ID(),
		ctx:     ctx,
		cancel:  cancel,
		peer:    peer,
		sender:  sender,
		buffer:  buffer,
		track:   sender.Track(),
		payload: sender.Track().PayloadType(),
	}

	// TODO: set channel type

	return f
}

// should be identical to peer id
func (f *SimpleForwarder) ID() string {
	return f.id
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
	// TODO: on close handler
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

func (f *SimpleForwarder) WriteRTCP(pkts []rtcp.Packet) error {
	return f.peer.conn.WriteRTCP(pkts)
}

// rtcpWorker receives RTCP packets from the destination peer
// this include packet loss packets
func (f *SimpleForwarder) rtcpWorker() {
	for {
		pkts, err := f.sender.ReadRTCP()
		if err == io.ErrClosedPipe {
			// TODO: handle peer closed
			//if recv := s.router.GetReceiver(0); recv != nil {
			//	recv.DeleteSender(s.id)
			//}
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
			if err := f.WriteRTCP(fwdPkts); err != nil {
				// TODO: log error
				//log.Errorf("Forwarding rtcp from sender err: %v", err)
			}
		}
	}
}
