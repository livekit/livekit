package rtc_test

import (
	"github.com/livekit/livekit-server/pkg/rtc/rtcfakes"
	"github.com/livekit/livekit-server/pkg/utils"
	"github.com/livekit/livekit-server/proto/livekit"
)

func newMockParticipant(name string) *rtcfakes.FakeParticipant {
	p := &rtcfakes.FakeParticipant{}
	p.IDReturns(utils.NewGuid(utils.ParticipantPrefix))
	p.NameReturns(name)
	p.StateReturns(livekit.ParticipantInfo_JOINED)

	return p
}
