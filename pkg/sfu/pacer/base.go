package pacer

import (
	"errors"
	"io"
	"time"

	"github.com/livekit/protocol/logger"
	"github.com/pion/rtp"
	"go.uber.org/atomic"
)

type Base struct {
	logger logger.Logger

	// for throttling error logs
	writeIOErrors atomic.Uint32
}

func NewBase(logger logger.Logger) *Base {
	return &Base{
		logger: logger,
	}
}

func (b *Base) SendPacket(p *Packet) error {
	sendingAt, err := b.writeRTPHeaderExtensions(p)
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

	if p.OnSent != nil {
		p.OnSent(p.Metadata, p.Header, len(p.Payload), sendingAt)
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

	sendingAt := time.Now()
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

	// SSBWE-TODO - add transport wide extension as necessary
	return sendingAt, nil
}

// ------------------------------------------------
