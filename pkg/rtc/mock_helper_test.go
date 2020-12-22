package rtc

import (
	"github.com/golang/mock/gomock"

	"github.com/livekit/livekit-server/pkg/utils"
	"github.com/livekit/livekit-server/proto/livekit"
)

func newMockPeerConnection(mockCtrl *gomock.Controller) *MockPeerConnection {
	pc := NewMockPeerConnection(mockCtrl)
	pc.EXPECT().OnDataChannel(gomock.Any()).AnyTimes()
	pc.EXPECT().OnICECandidate(gomock.Any()).AnyTimes()
	pc.EXPECT().OnICEConnectionStateChange(gomock.Any()).AnyTimes()
	pc.EXPECT().OnNegotiationNeeded(gomock.Any()).AnyTimes()
	pc.EXPECT().OnTrack(gomock.Any()).AnyTimes()
	return pc
}

func newMockSignalConnection(mockCtrl *gomock.Controller) *MockSignalConnection {
	sc := NewMockSignalConnection(mockCtrl)
	return sc
}

func newMockParticipant(mockCtrl *gomock.Controller) *MockParticipant {
	p := NewMockParticipant(mockCtrl)
	p.EXPECT().ID().Return(utils.NewGuid(utils.ParticipantPrefix)).AnyTimes()
	p.EXPECT().Name().Return("tester").AnyTimes()
	p.EXPECT().State().Return(livekit.ParticipantInfo_JOINED).AnyTimes()

	// default callbacks
	p.EXPECT().OnICECandidate(gomock.Any()).AnyTimes()
	p.EXPECT().OnClose(gomock.Any()).AnyTimes()
	p.EXPECT().OnOffer(gomock.Any()).AnyTimes()
	p.EXPECT().OnStateChange(gomock.Any()).AnyTimes()
	p.EXPECT().OnTrackPublished(gomock.Any()).AnyTimes()
	return p
}
