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

package sendsidebwe

import (
	"go.uber.org/zap/zapcore"
)

type packetInfo struct {
	sequenceNumber uint64
	sendTime       int64
	recvTime       int64
	size           uint16
	isRTX          bool
	// SSBWE-TODO: possibly add the following fields - pertaining to this packet,
	// idea is to be able to figure out probe start/end and check for bitrate in that window
}

func (pi *packetInfo) Reset(sequenceNumber uint64) {
	pi.sequenceNumber = sequenceNumber
	pi.sendTime = 0
	pi.recvTime = 0
	pi.size = 0
	pi.isRTX = false
}

func (pi *packetInfo) MarshalLogObject(e zapcore.ObjectEncoder) error {
	if pi == nil {
		return nil
	}

	e.AddUint64("sequenceNumber", pi.sequenceNumber)
	e.AddInt64("sendTime", pi.sendTime)
	e.AddInt64("recvTime", pi.recvTime)
	e.AddUint16("size", pi.size)
	e.AddBool("isRTX", pi.isRTX)
	return nil
}
