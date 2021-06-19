package rtc_test

import (
	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/rtc/types/typesfakes"
	livekit "github.com/livekit/livekit-server/proto"
	"github.com/livekit/protocol/utils"
)

func newMockParticipant(identity string, protocol types.ProtocolVersion) *typesfakes.FakeParticipant {
	p := &typesfakes.FakeParticipant{}
	p.IDReturns(utils.NewGuid(utils.ParticipantPrefix))
	p.IdentityReturns(identity)
	p.StateReturns(livekit.ParticipantInfo_JOINED)
	p.ProtocolVersionReturns(protocol)
	p.CanSubscribeReturns(true)
	p.CanPublishReturns(true)

	p.SetMetadataStub = func(m string) {
		var f func(participant types.Participant)
		if p.OnMetadataUpdateCallCount() > 0 {
			f = p.OnMetadataUpdateArgsForCall(p.OnMetadataUpdateCallCount() - 1)
		}
		if f != nil {
			f(p)
		}
	}
	updateTrack := func() {
		var f func(participant types.Participant, track types.PublishedTrack)
		if p.OnTrackUpdatedCallCount() > 0 {
			f = p.OnTrackUpdatedArgsForCall(p.OnTrackUpdatedCallCount() - 1)
		}
		if f != nil {
			f(p, nil)
		}
	}

	p.SetTrackMutedStub = func(sid string, muted bool) {
		updateTrack()
	}
	p.AddTrackStub = func(req *livekit.AddTrackRequest) {
		updateTrack()
	}

	return p
}

func newMockTrack(kind livekit.TrackType, name string) *typesfakes.FakePublishedTrack {
	t := &typesfakes.FakePublishedTrack{}
	t.IDReturns(utils.NewGuid(utils.TrackPrefix))
	t.KindReturns(kind)
	t.NameReturns(name)
	return t
}
