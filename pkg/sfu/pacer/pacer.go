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
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
)

type ExtensionData struct {
	ID      uint8
	Payload []byte
}

type Packet struct {
	Header             *rtp.Header
	Extensions         []ExtensionData
	Payload            []byte
	AbsSendTimeExtID   uint8
	TransportWideExtID uint8
	WriteStream        webrtc.TrackLocalWriter
	Metadata           interface{}
	OnSent             func(md interface{}, sentHeader *rtp.Header, payloadSize int, sentTime time.Time, sendError error)
}

type Pacer interface {
	Enqueue(p Packet)
	Stop()

	SetInterval(interval time.Duration)
	SetBitrate(bitrate int)
}

// ------------------------------------------------
