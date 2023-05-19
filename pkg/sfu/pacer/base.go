package pacer

import (
	"errors"
	"io"
	"time"

	"github.com/livekit/livekit-server/pkg/sfu/sendsidebwe"
	"github.com/livekit/protocol/logger"
	"github.com/pion/rtp"
	"go.uber.org/atomic"
)

type Base struct {
	logger logger.Logger

	packetTime  *PacketTime
	sendSideBWE *sendsidebwe.SendSideBWE

	// for throttling error logs
	writeIOErrors atomic.Uint32
}

func NewBase(logger logger.Logger, sendSideBWE *sendsidebwe.SendSideBWE) *Base {
	return &Base{
		logger:      logger,
		packetTime:  NewPacketTime(),
		sendSideBWE: sendSideBWE,
	}
}

func (b *Base) SendPacket(p *Packet) error {
	var sendingAt time.Time
	var err error
	defer func() {
		if p.OnSent != nil {
			p.OnSent(p.Metadata, p.Header, len(p.Payload), sendingAt, err)
		}
	}()

	sendingAt, twSN, err := b.writeRTPHeaderExtensions(p)
	if err != nil {
		b.logger.Errorw("writing rtp header extensions err", err)
		return err
	}

	_, err = p.WriteStream.WriteRTP(p.Header, p.Payload)
	if err != nil {
		if errors.Is(err, io.ErrClosedPipe) {
			writeIOErrors := b.writeIOErrors.Inc()
			if (writeIOErrors % 100) == 1 {
				b.logger.Errorw("write rtp packet failed", err, "count", writeIOErrors)
			}
		} else {
			b.logger.Errorw("write rtp packet failed", err)
		}
		return err
	}

	if p.AbsSendTimeExtID != 0 {
		b.sendSideBWE.PacketSent(sendingAt, twSN, p.Header.MarshalSize(), len(p.Payload))
	}

	return nil
}

// writes RTP header extensions of track
func (b *Base) writeRTPHeaderExtensions(p *Packet) (time.Time, uint16, error) {
	// clear out extensions that may have been in the forwarded header
	p.Header.Extension = false
	p.Header.ExtensionProfile = 0
	p.Header.Extensions = []rtp.Extension{}

	for _, ext := range p.Extensions {
		if ext.ID == 0 || len(ext.Payload) == 0 {
			continue
		}

		p.Header.SetExtension(ext.ID, ext.Payload)
	}

	sendingAt := b.packetTime.Get()
	if p.AbsSendTimeExtID != 0 {
		sendTime := rtp.NewAbsSendTimeExtension(sendingAt)
		b, err := sendTime.Marshal()
		if err != nil {
			return time.Time{}, 0, err
		}

		err = p.Header.SetExtension(p.AbsSendTimeExtID, b)
		if err != nil {
			return time.Time{}, 0, err
		}
	}

	twSN := uint16(0)
	if p.TransportWideExtID != 0 {
		twSN = b.sendSideBWE.GetNext()
		tw := rtp.TransportCCExtension{
			TransportSequence: twSN,
		}
		b, err := tw.Marshal()
		if err != nil {
			return time.Time{}, 0, err
		}

		err = p.Header.SetExtension(p.TransportWideExtID, b)
		if err != nil {
			return time.Time{}, 0, err
		}
	}

	return sendingAt, twSN, nil
}

// ------------------------------------------------
