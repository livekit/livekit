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

package wire

import (
	"encoding/binary"
	"fmt"
	"io"

	"google.golang.org/protobuf/proto"
)

// MaxControlMessageSize bounds one framed control message so a malformed length
// prefix cannot make the reader allocate without bound.
const MaxControlMessageSize = 1 << 20

// WriteControlMessage writes a length-delimited protobuf on the control stream:
// a 4-byte big-endian length followed by the marshaled message. The stream
// carries the WorkerMessage/ServerMessage exchange.
func WriteControlMessage(w io.Writer, m proto.Message) error {
	return writeLenPrefixed(w, m)
}

// ReadControlMessage reads one length-delimited protobuf written by
// WriteControlMessage into m.
func ReadControlMessage(r io.Reader, m proto.Message) error {
	return readLenPrefixed(r, m, MaxControlMessageSize)
}

// writeLenPrefixed is the package's one length-delimited protobuf encoding:
// 4-byte big-endian length, then the marshaled message. The control stream and
// the per-exchange preamble share it.
func writeLenPrefixed(w io.Writer, m proto.Message) error {
	b, err := proto.Marshal(m)
	if err != nil {
		return err
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(b)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

func readLenPrefixed(r io.Reader, m proto.Message, max uint32) error {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > max {
		return fmt.Errorf("message too large: %d bytes", n)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return err
	}
	return proto.Unmarshal(b, m)
}
