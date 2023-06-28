package pacer

import (
	"errors"
	"io"
	"time"

	"github.com/livekit/protocol/logger"
	"github.com/pion/rtp"
)

type Base struct {
	logger logger.Logger

	packetTime *PacketTime
}

func NewBase(logger logger.Logger) *Base {
	return &Base{
		logger:     logger,
		packetTime: NewPacketTime(),
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

	sendingAt, err = b.writeRTPHeaderExtensions(p)
	if err != nil {
		b.logger.Errorw("writing rtp header extensions err", err)
		return err
	}

	_, err = p.WriteStream.WriteRTP(p.Header, p.Payload)
	if err != nil {
		if !errors.Is(err, io.ErrClosedPipe) {
			b.logger.Errorw("write rtp packet failed", err)
		}
		return err
	}

	return nil
}

// writes RTP header extensions of track
func (b *Base) writeRTPHeaderExtensions(p *Packet) (time.Time, error) {
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
			return time.Time{}, err
		}

		err = p.Header.SetExtension(p.AbsSendTimeExtID, b)
		if err != nil {
			return time.Time{}, err
		}
	}

	return sendingAt, nil
}

// ------------------------------------------------
