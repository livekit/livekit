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

package types

type ProtocolVersion int

const CurrentProtocol = 16

func (v ProtocolVersion) SupportsPackedStreamId() bool {
	return v > 0
}

func (v ProtocolVersion) SupportsProtobuf() bool {
	return v > 0
}

func (v ProtocolVersion) HandlesDataPackets() bool {
	return v > 1
}

// SubscriberAsPrimary indicates clients initiate subscriber connection as primary
func (v ProtocolVersion) SubscriberAsPrimary() bool {
	return v > 2
}

// SupportsSpeakerChanged - if client handles speaker info deltas, instead of a comprehensive list
func (v ProtocolVersion) SupportsSpeakerChanged() bool {
	return v > 2
}

// SupportsTransceiverReuse - if transceiver reuse is supported, optimizes SDP size
func (v ProtocolVersion) SupportsTransceiverReuse() bool {
	return v > 3
}

// SupportsConnectionQuality - avoid sending frequent ConnectionQuality updates for lower protocol versions
func (v ProtocolVersion) SupportsConnectionQuality() bool {
	return v > 4
}

func (v ProtocolVersion) SupportsSessionMigrate() bool {
	return v > 5
}

func (v ProtocolVersion) SupportsICELite() bool {
	return v > 5
}

func (v ProtocolVersion) SupportsUnpublish() bool {
	return v > 6
}

// SupportFastStart - if client supports fast start, server side will send media streams
// in the first offer
func (v ProtocolVersion) SupportFastStart() bool {
	return v > 7
}

func (v ProtocolVersion) SupportsDisconnectedUpdate() bool {
	return v > 8
}

func (v ProtocolVersion) SupportsSyncStreamID() bool {
	return v > 9
}

func (v ProtocolVersion) SupportsConnectionQualityLost() bool {
	return v > 10
}

func (v ProtocolVersion) SupportsAsyncRoomID() bool {
	return v > 11
}

func (v ProtocolVersion) SupportsIdentityBasedReconnection() bool {
	return v > 11
}

func (v ProtocolVersion) SupportsRegionsInLeaveRequest() bool {
	return v > 12
}

func (v ProtocolVersion) SupportsNonErrorSignalResponse() bool {
	return v > 14
}

func (v ProtocolVersion) SupportsMoving() bool {
	return v > 15
}
