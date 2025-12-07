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

package client

import (
	"context"
	"math/rand"
	"time"

	"github.com/livekit/livekit-server/pkg/rtc/datatrack"
	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/protocol/logger"
)

type dataTrackWriter struct {
	ctx       context.Context
	cancel    context.CancelFunc
	handle    uint16
	transport types.DataTrackTransport
}

func NewDataTrackWriter(ctx context.Context, handle uint16, transport types.DataTrackTransport) TrackWriter {
	ctx, cancel := context.WithCancel(ctx)
	return &dataTrackWriter{
		ctx:       ctx,
		cancel:    cancel,
		handle:    handle,
		transport: transport,
	}
}

func (d *dataTrackWriter) Start() error {
	go d.writeFrames()
	return nil
}

func (d *dataTrackWriter) Stop() {
	d.cancel()
}

func (d *dataTrackWriter) writeFrames() {
	seqNum := uint16(0)
	frameNum := uint16(0)
	for {
		select {
		case <-d.ctx.Done():
			return

		default:
			packets := datatrack.GenerateRawDataPackets(d.handle, seqNum, frameNum, 1, rand.Intn(2048)+1, 100*time.Millisecond)
			for _, packet := range packets {
				if err := d.transport.SendDataTrackMessage(packet); err != nil {
					logger.Errorw("could not send data track packet", err)
				}
			}

			if len(packets) != 0 {
				var lastPacket datatrack.Packet
				if err := lastPacket.Unmarshal(packets[len(packets)-1]); err == nil {
					seqNum = lastPacket.SequenceNumber + 1
					frameNum = lastPacket.FrameNumber + 1
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
}
