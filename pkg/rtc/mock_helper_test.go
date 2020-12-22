package rtc

import (
	"github.com/golang/mock/gomock"
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
