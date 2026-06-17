// Copyright 2026 LiveKit, Inc.
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

package service

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
)

const (
	moqWireProtocol = "livekit-moq-h264/1"
	moqWireVersion  = 1
)

var moqWireMagic = [4]byte{'L', 'K', 'M', 'Q'}

type moqWireMessage struct {
	Type            string                `json:"type"`
	Protocol        string                `json:"protocol,omitempty"`
	Room            string                `json:"room,omitempty"`
	TrackID         string                `json:"trackId,omitempty"`
	TrackName       string                `json:"trackName,omitempty"`
	PublisherID     string                `json:"publisherId,omitempty"`
	Publisher       string                `json:"publisher,omitempty"`
	MimeType        string                `json:"mimeType,omitempty"`
	PayloadFormat   string                `json:"payloadFormat,omitempty"`
	Width           uint32                `json:"width,omitempty"`
	Height          uint32                `json:"height,omitempty"`
	Layer           int32                 `json:"layer,omitempty"`
	Sequence        uint64                `json:"sequence,omitempty"`
	RTPTime         uint32                `json:"rtpTime,omitempty"`
	KeyFrame        bool                  `json:"keyFrame,omitempty"`
	Cached          bool                  `json:"cached,omitempty"`
	SentAtUnixNs    int64                 `json:"sentAtUnixNs,omitempty"`
	UserTimestampUs *int64                `json:"userTimestampUs,omitempty"`
	FrameID         *uint32               `json:"frameId,omitempty"`
	Tracks          []moqWireCatalogTrack `json:"tracks,omitempty"`
	Error           string                `json:"error,omitempty"`
}

type moqWireCatalogTrack struct {
	TrackID     string `json:"trackId"`
	TrackName   string `json:"trackName,omitempty"`
	PublisherID string `json:"publisherId,omitempty"`
	Publisher   string `json:"publisher,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
	Width       uint32 `json:"width,omitempty"`
	Height      uint32 `json:"height,omitempty"`
}

func writeMoQWireMessage(w io.Writer, msg moqWireMessage, payload []byte) error {
	meta, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	if uint64(len(meta)) > uint64(math.MaxUint32) {
		return fmt.Errorf("moq metadata too large: %d", len(meta))
	}
	if uint64(len(payload)) > uint64(math.MaxUint32) {
		return fmt.Errorf("moq payload too large: %d", len(payload))
	}

	var header [16]byte
	copy(header[0:4], moqWireMagic[:])
	header[4] = moqWireVersion
	binary.BigEndian.PutUint32(header[8:12], uint32(len(meta)))
	binary.BigEndian.PutUint32(header[12:16], uint32(len(payload)))
	if _, err := w.Write(header[:]); err != nil {
		return err
	}
	if _, err := w.Write(meta); err != nil {
		return err
	}
	if len(payload) != 0 {
		_, err = w.Write(payload)
	}
	return err
}
