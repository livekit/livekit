// Copyright 2024 LiveKit, Inc.
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

package rtc

import (
	"testing"

	"github.com/pion/sdp/v3"
	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/logger"
)

func TestPublisherFlexFECRepairPairsFromSDP(t *testing.T) {
	const offerSDP = "v=0\r\n" +
		"o=- 0 0 IN IP4 127.0.0.1\r\n" +
		"s=-\r\n" +
		"t=0 0\r\n" +
		"m=video 9 UDP/TLS/RTP/SAVPF 96 49\r\n" +
		"c=IN IP4 0.0.0.0\r\n" +
		"a=mid:0\r\n" +
		"a=sendonly\r\n" +
		"a=rtpmap:96 VP8/90000\r\n" +
		"a=rtpmap:49 flexfec-03/90000\r\n" +
		"a=ssrc:1111 cname:publisher\r\n" +
		"a=ssrc:2222 cname:publisher\r\n" +
		"a=ssrc-group:FEC-FR 1111 2222\r\n" +
		"m=video 9 UDP/TLS/RTP/SAVPF 96 49\r\n" +
		"c=IN IP4 0.0.0.0\r\n" +
		"a=mid:1\r\n" +
		"a=sendonly\r\n" +
		"a=rid:high send\r\n" +
		"a=rtpmap:96 VP8/90000\r\n" +
		"a=rtpmap:49 flexfec-03/90000\r\n" +
		"a=ssrc:3333 cname:publisher\r\n" +
		"a=ssrc:4444 cname:publisher\r\n" +
		"a=ssrc-group:FEC-FR 3333 4444\r\n"

	var sd sdp.SessionDescription
	require.NoError(t, sd.UnmarshalString(offerSDP))

	require.Equal(t,
		map[uint32]uint32{2222: 1111},
		nonSimulcastFECRepairsFromSDP(&sd, logger.GetLogger()),
	)
}
