package mesh

import (
	"sync"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"

	"github.com/livekit/protocol/logger"
	"github.com/livekit/psrpc"
)

// Receiver consumes RTP/RTCP from a remote node via a psrpc stream.
type Receiver struct {
	stream psrpc.Stream[*RTPMessage, *RTCPMessage]
	logger logger.Logger
	mu     sync.Mutex

	OnRTP  func(*rtp.Packet)
	OnRTCP func([]rtcp.Packet)
}

func NewReceiver(stream psrpc.Stream[*RTPMessage, *RTCPMessage], l logger.Logger) *Receiver {
	r := &Receiver{stream: stream, logger: l}
	go r.read()
	return r
}

func (r *Receiver) read() {
	for msg := range r.stream.Channel() {
		if msg.Packets != nil { // RTCP
			pkts := make([]rtcp.Packet, 0, len(msg.Packets))
			for _, b := range msg.Packets {
				ps, err := rtcp.Unmarshal(b)
				if err == nil {
					pkts = append(pkts, ps...)
				}
			}
			if r.OnRTCP != nil {
				r.OnRTCP(pkts)
			}
			continue
		}
		if msg.Packet != nil {
			pkt := &rtp.Packet{}
			if err := pkt.Unmarshal(msg.Packet); err == nil {
				if r.OnRTP != nil {
					r.OnRTP(pkt)
				}
			}
		}
	}
}

func (r *Receiver) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stream.Close(nil)
}
