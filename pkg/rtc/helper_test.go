package rtc_test

import (
	"github.com/livekit/livekit-server/pkg/rtc/types/typesfakes"
	"github.com/livekit/livekit-server/pkg/utils"
	"github.com/livekit/livekit-server/proto/livekit"
)

func newMockParticipant(identity string) *typesfakes.FakeParticipant {
	p := &typesfakes.FakeParticipant{}
	p.IDReturns(utils.NewGuid(utils.ParticipantPrefix))
	p.IdentityReturns(identity)
	p.StateReturns(livekit.ParticipantInfo_JOINED)

	return p
}

func newMockTrack(kind livekit.TrackType, name string) *typesfakes.FakePublishedTrack {
	t := &typesfakes.FakePublishedTrack{}
	t.IDReturns(utils.NewGuid(utils.TrackPrefix))
	t.KindReturns(kind)
	t.NameReturns(name)
	return t
}
