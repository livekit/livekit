package rtc_test

import (
	"github.com/livekit/livekit-server/pkg/rtc/types/typesfakes"
	"github.com/livekit/livekit-server/pkg/utils"
	"github.com/livekit/livekit-server/proto/livekit"
)

func newMockParticipant(name string) *typesfakes.FakeParticipant {
	p := &typesfakes.FakeParticipant{}
	p.IDReturns(utils.NewGuid(utils.ParticipantPrefix))
	p.NameReturns(name)
	p.StateReturns(livekit.ParticipantInfo_JOINED)

	return p
}

func newMockTrack(kind livekit.TrackInfo_Type, name string) *typesfakes.FakePublishedTrack {
	t := &typesfakes.FakePublishedTrack{}
	t.IDReturns(utils.NewGuid(utils.TrackPrefix))
	t.KindReturns(kind)
	t.StreamIDReturns(name)
	return t
}
