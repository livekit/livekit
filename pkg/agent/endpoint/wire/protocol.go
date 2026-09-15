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

// CurrentProtocol is the data-plane protocol version this implementation speaks,
// negotiated in RegisterWorkerResponse.
const CurrentProtocol uint32 = 1

// MinProtocol is the oldest data-plane protocol version still served.
const MinProtocol uint32 = 1

// SessionCloseOK is the WebTransport application close code for a normal session
// teardown; the human-readable reason travels in the close message.
const SessionCloseOK = 0
