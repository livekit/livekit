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

package pacer

import (
	"errors"
	"io"
	"time"

	"github.com/livekit/livekit-server/pkg/sfu/sendsidebwe"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils/mono"
	"github.com/pion/rtp"
)

type Base struct {
	logger logger.Logger

	sendSideBWE *sendsidebwe.SendSideBWE
}

func NewBase(logger logger.Logger, sendSideBWE *sendsidebwe.SendSideBWE) *Base {
	return &Base{
		logger:      logger,
		sendSideBWE: sendSideBWE,
	}
}

func (b *Base) SetInterval(_interval time.Duration) {
}

func (b *Base) SetBitrate(_bitrate int) {
}

func (b *Base) SendPacket(p *Packet) (int, error) {
	defer func() {
		if p.Pool != nil && p.PoolEntity != nil {
			p.Pool.Put(p.PoolEntity)
		}
	}()

	err := b.patchRTPHeaderExtensions(p)
	if err != nil {
		b.logger.Errorw("writing rtp header extensions err", err)
		return 0, err
	}

	var written int
	written, err = p.WriteStream.WriteRTP(p.Header, p.Payload)
	if err != nil {
		if !errors.Is(err, io.ErrClosedPipe) {
			b.logger.Errorw("write rtp packet failed", err)
		}
		return 0, err
	}

	return written, nil
}

// patch just abs-send-time and transport-cc extensions if applicable
func (b *Base) patchRTPHeaderExtensions(p *Packet) error {
	sendingAt := mono.Now()
	if p.AbsSendTimeExtID != 0 {
		sendTime := rtp.NewAbsSendTimeExtension(sendingAt)
		b, err := sendTime.Marshal()
		if err != nil {
			return err
		}

		if err = p.Header.SetExtension(p.AbsSendTimeExtID, b); err != nil {
			return err
		}
	}

	if p.TransportWideExtID != 0 && b.sendSideBWE != nil {
		twccSN := b.sendSideBWE.RecordPacketSendAndGetSequenceNumber(
			sendingAt,
			p.Header.MarshalSize()+len(p.Payload),
			p.IsRTX,
		)
		twccExt := rtp.TransportCCExtension{
			TransportSequence: twccSN,
		}
		b, err := twccExt.Marshal()
		if err != nil {
			return err
		}

		if err = p.Header.SetExtension(p.TransportWideExtID, b); err != nil {
			return err
		}
	}

	return nil
}

// ------------------------------------------------
