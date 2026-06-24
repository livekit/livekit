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
	"bytes"
	"fmt"
	"io"

	"github.com/quic-go/quic-go/quicvarint"
)

type moqLitePublishGroup struct {
	sequence uint64
	payload  []byte
}

func readMoQLitePublishGroup(r io.Reader, maxPayloadBytes int) (moqLitePublishGroup, error) {
	var group moqLitePublishGroup
	reader := quicvarint.NewReader(r)
	streamType, err := quicvarint.Read(reader)
	if err != nil {
		return group, err
	}
	if streamType != uint64(moqLiteStreamGroup) {
		return group, fmt.Errorf("unexpected moq-lite publish stream type: %d", streamType)
	}

	groupMeta, err := readMoQLiteMessageLimited(reader, moqLiteMaxMessageSize)
	if err != nil {
		return group, err
	}
	br := bytes.NewReader(groupMeta)
	metaReader := quicvarint.NewReader(br)
	if _, err = quicvarint.Read(metaReader); err != nil {
		return group, err
	}
	group.sequence, err = quicvarint.Read(metaReader)
	if err != nil {
		return group, err
	}
	if br.Len() != 0 {
		return group, fmt.Errorf("moq-lite publish group had %d trailing bytes", br.Len())
	}

	payloadSize, err := quicvarint.Read(reader)
	if err != nil {
		return group, err
	}
	if payloadSize > uint64(maxPayloadBytes) {
		return group, fmt.Errorf("moq-lite publish payload too large: %d", payloadSize)
	}
	group.payload = make([]byte, int(payloadSize))
	_, err = io.ReadFull(reader, group.payload)
	return group, err
}

func readMoQLiteMessageLimited(r quicvarint.Reader, maxSize int) ([]byte, error) {
	size, err := quicvarint.Read(r)
	if err != nil {
		return nil, err
	}
	if size > uint64(maxSize) {
		return nil, fmt.Errorf("moq-lite message too large: %d", size)
	}
	data := make([]byte, int(size))
	if _, err := io.ReadFull(r, data); err != nil {
		return nil, err
	}
	return data, nil
}
