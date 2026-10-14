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
	"github.com/quic-go/webtransport-go"

	"github.com/livekit/protocol/livekit"
)

// StreamCode converts a protocol reset code to the WebTransport code that
// carries it. HSR_ABORT is zero, so a teardown with nothing to say sends the
// plain cancel code.
func StreamCode(c livekit.AgentHttp_HttpStreamResetCode) webtransport.StreamErrorCode {
	return webtransport.StreamErrorCode(c)
}
