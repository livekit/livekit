package rtc

import (
	"io"
	"sync"

	"github.com/pion/rtp"

	"github.com/livekit/livekit-server/pkg/logger"
	"github.com/livekit/livekit-server/pkg/sfu"

	"context"

	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"
)

const (
	// TODO: could probably increase this depending on Configuration/memory
	maxChanSize = 1024
)

// A receiver is responsible for pulling from a track
type Receiver struct {
	peerId      string
	ctx         context.Context
	cancel      context.CancelFunc
	rtpReceiver *webrtc.RTPReceiver
	track       *webrtc.Track
	buffer      *sfu.Buffer
	rtpChan     chan *rtp.Packet
	once        sync.Once

	onCloseHandler func(r *Receiver)
}

func NewReceiver(ctx context.Context, peerId string, rtpReceiver *webrtc.RTPReceiver, conf ReceiverConfig, tccExt int) *Receiver {
	ctx, cancel := context.WithCancel(ctx)
	track := rtpReceiver.Track()
	return &Receiver{
		ctx:         ctx,
		cancel:      cancel,
		peerId:      peerId,
		rtpReceiver: rtpReceiver,
		track:       track,
		rtpChan:     make(chan *rtp.Packet, maxChanSize),
		buffer: sfu.NewBuffer(track, sfu.BufferOptions{
			BufferTime: conf.maxBufferTime,
			MaxBitRate: conf.maxBandwidth * 1000,
			TCCExt:     tccExt,
		}),
		once: sync.Once{},
	}
}

func (r *Receiver) PeerId() string {
	return r.peerId
}

func (r *Receiver) TrackId() string {
	return r.track.ID()
}

// starts reading RTP and push to buffer
func (r *Receiver) Start() {
	r.once.Do(func() {
		go r.rtpWorker()
		go r.rtcpWorker()
	})
}

// Close gracefully close the track. if the context is canceled
func (r *Receiver) Close() {
	if r.ctx.Err() != nil {
		return
	}
	r.cancel()
}

// returns channel to read rtp packets
func (r *Receiver) RTPChan() <-chan *rtp.Packet {
	return r.rtpChan
}

// Builds RTCP report from buffer, report indicates receiving quality
func (r *Receiver) BuildRTCP() (rtcp.ReceptionReport, []rtcp.Packet) {
	return r.buffer.BuildRTCP()
}

// WriteBufferedPacket writes buffered packet to track, return error if packet not found
func (r *Receiver) WriteBufferedPacket(sn uint16, track *webrtc.Track, snOffset uint16, tsOffset, ssrc uint32) error {
	if r.buffer == nil || r.ctx.Err() != nil {
		return nil
	}
	return r.buffer.WritePacket(sn, track, snOffset, tsOffset, ssrc)
}

// rtpWorker reads RTP stream, fills buffer and channel
func (r *Receiver) rtpWorker() {
	defer func() {
		close(r.rtpChan)
		if r.onCloseHandler != nil {
			r.onCloseHandler(r)
		}
	}()

	for {
		pkt, err := r.track.ReadRTP()
		if err == io.EOF {
			r.Close()
			return
		}

		if err != nil {
			// log and continue
			logger.GetLogger().Warnw("receiver error reading RTP",
				"peer", r.peerId,
				"track", r.track.SSRC(),
				"err", err,
			)
			continue
		}

		r.buffer.Push(pkt)

		select {
		case <-r.ctx.Done():
			return
		default:
			r.rtpChan <- pkt
		}
	}
}

// rtcpWorker reads RTCP messages from receiver, notifies buffer
func (r *Receiver) rtcpWorker() {
	for {
		pkts, err := r.rtpReceiver.ReadRTCP()
		if err == io.ErrClosedPipe || r.ctx.Err() != nil {
			return
		}
		if err != nil {
			logger.GetLogger().Warnw("receiver error reading RTCP",
				"peer", r.peerId,
				"track", r.track.SSRC(),
				"err", err,
			)
			continue
		}

		for _, pkt := range pkts {
			switch pkt := pkt.(type) {
			case *rtcp.SenderReport:
				r.buffer.SetSenderReportData(pkt.RTPTime, pkt.NTPTime)
			}
		}
	}
}
