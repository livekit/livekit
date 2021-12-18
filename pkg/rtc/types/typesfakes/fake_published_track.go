// Code generated by counterfeiter. DO NOT EDIT.
package typesfakes

import (
	"sync"

	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/sfu"
	"github.com/livekit/protocol/livekit"
)

type FakePublishedTrack struct {
	AddOnCloseStub        func(func())
	addOnCloseMutex       sync.RWMutex
	addOnCloseArgsForCall []struct {
		arg1 func()
	}
	AddSubscriberStub        func(types.Participant) error
	addSubscriberMutex       sync.RWMutex
	addSubscriberArgsForCall []struct {
		arg1 types.Participant
	}
	addSubscriberReturns struct {
		result1 error
	}
	addSubscriberReturnsOnCall map[int]struct {
		result1 error
	}
	GetQualityForDimensionStub        func(uint32, uint32) livekit.VideoQuality
	getQualityForDimensionMutex       sync.RWMutex
	getQualityForDimensionArgsForCall []struct {
		arg1 uint32
		arg2 uint32
	}
	getQualityForDimensionReturns struct {
		result1 livekit.VideoQuality
	}
	getQualityForDimensionReturnsOnCall map[int]struct {
		result1 livekit.VideoQuality
	}
	IDStub        func() string
	iDMutex       sync.RWMutex
	iDArgsForCall []struct {
	}
	iDReturns struct {
		result1 string
	}
	iDReturnsOnCall map[int]struct {
		result1 string
	}
	IsMutedStub        func() bool
	isMutedMutex       sync.RWMutex
	isMutedArgsForCall []struct {
	}
	isMutedReturns struct {
		result1 bool
	}
	isMutedReturnsOnCall map[int]struct {
		result1 bool
	}
	IsSimulcastStub        func() bool
	isSimulcastMutex       sync.RWMutex
	isSimulcastArgsForCall []struct {
	}
	isSimulcastReturns struct {
		result1 bool
	}
	isSimulcastReturnsOnCall map[int]struct {
		result1 bool
	}
	IsSubscriberStub        func(string) bool
	isSubscriberMutex       sync.RWMutex
	isSubscriberArgsForCall []struct {
		arg1 string
	}
	isSubscriberReturns struct {
		result1 bool
	}
	isSubscriberReturnsOnCall map[int]struct {
		result1 bool
	}
	KindStub        func() livekit.TrackType
	kindMutex       sync.RWMutex
	kindArgsForCall []struct {
	}
	kindReturns struct {
		result1 livekit.TrackType
	}
	kindReturnsOnCall map[int]struct {
		result1 livekit.TrackType
	}
	NameStub        func() string
	nameMutex       sync.RWMutex
	nameArgsForCall []struct {
	}
	nameReturns struct {
		result1 string
	}
	nameReturnsOnCall map[int]struct {
		result1 string
	}
	NotifySubscriberMaxQualityStub        func(string, livekit.VideoQuality)
	notifySubscriberMaxQualityMutex       sync.RWMutex
	notifySubscriberMaxQualityArgsForCall []struct {
		arg1 string
		arg2 livekit.VideoQuality
	}
	NotifySubscriberMuteStub        func(string)
	notifySubscriberMuteMutex       sync.RWMutex
	notifySubscriberMuteArgsForCall []struct {
		arg1 string
	}
	NumUpTracksStub        func() (uint32, uint32)
	numUpTracksMutex       sync.RWMutex
	numUpTracksArgsForCall []struct {
	}
	numUpTracksReturns struct {
		result1 uint32
		result2 uint32
	}
	numUpTracksReturnsOnCall map[int]struct {
		result1 uint32
		result2 uint32
	}
	OnSubscribedMaxQualityChangeStub        func(func(trackSid string, subscribedQualities []*livekit.SubscribedQuality) error)
	onSubscribedMaxQualityChangeMutex       sync.RWMutex
	onSubscribedMaxQualityChangeArgsForCall []struct {
		arg1 func(trackSid string, subscribedQualities []*livekit.SubscribedQuality) error
	}
	PublishLossPercentageStub        func() uint32
	publishLossPercentageMutex       sync.RWMutex
	publishLossPercentageArgsForCall []struct {
	}
	publishLossPercentageReturns struct {
		result1 uint32
	}
	publishLossPercentageReturnsOnCall map[int]struct {
		result1 uint32
	}
	ReceiverStub        func() sfu.TrackReceiver
	receiverMutex       sync.RWMutex
	receiverArgsForCall []struct {
	}
	receiverReturns struct {
		result1 sfu.TrackReceiver
	}
	receiverReturnsOnCall map[int]struct {
		result1 sfu.TrackReceiver
	}
	RemoveAllSubscribersStub        func()
	removeAllSubscribersMutex       sync.RWMutex
	removeAllSubscribersArgsForCall []struct {
	}
	RemoveSubscriberStub        func(string)
	removeSubscriberMutex       sync.RWMutex
	removeSubscriberArgsForCall []struct {
		arg1 string
	}
	SdpCidStub        func() string
	sdpCidMutex       sync.RWMutex
	sdpCidArgsForCall []struct {
	}
	sdpCidReturns struct {
		result1 string
	}
	sdpCidReturnsOnCall map[int]struct {
		result1 string
	}
	SetMutedStub        func(bool)
	setMutedMutex       sync.RWMutex
	setMutedArgsForCall []struct {
		arg1 bool
	}
	SignalCidStub        func() string
	signalCidMutex       sync.RWMutex
	signalCidArgsForCall []struct {
	}
	signalCidReturns struct {
		result1 string
	}
	signalCidReturnsOnCall map[int]struct {
		result1 string
	}
	SourceStub        func() livekit.TrackSource
	sourceMutex       sync.RWMutex
	sourceArgsForCall []struct {
	}
	sourceReturns struct {
		result1 livekit.TrackSource
	}
	sourceReturnsOnCall map[int]struct {
		result1 livekit.TrackSource
	}
	ToProtoStub        func() *livekit.TrackInfo
	toProtoMutex       sync.RWMutex
	toProtoArgsForCall []struct {
	}
	toProtoReturns struct {
		result1 *livekit.TrackInfo
	}
	toProtoReturnsOnCall map[int]struct {
		result1 *livekit.TrackInfo
	}
	UpdateVideoLayersStub        func([]*livekit.VideoLayer)
	updateVideoLayersMutex       sync.RWMutex
	updateVideoLayersArgsForCall []struct {
		arg1 []*livekit.VideoLayer
	}
	invocations      map[string][][]interface{}
	invocationsMutex sync.RWMutex
}

func (fake *FakePublishedTrack) AddOnClose(arg1 func()) {
	fake.addOnCloseMutex.Lock()
	fake.addOnCloseArgsForCall = append(fake.addOnCloseArgsForCall, struct {
		arg1 func()
	}{arg1})
	stub := fake.AddOnCloseStub
	fake.recordInvocation("AddOnClose", []interface{}{arg1})
	fake.addOnCloseMutex.Unlock()
	if stub != nil {
		fake.AddOnCloseStub(arg1)
	}
}

func (fake *FakePublishedTrack) AddOnCloseCallCount() int {
	fake.addOnCloseMutex.RLock()
	defer fake.addOnCloseMutex.RUnlock()
	return len(fake.addOnCloseArgsForCall)
}

func (fake *FakePublishedTrack) AddOnCloseCalls(stub func(func())) {
	fake.addOnCloseMutex.Lock()
	defer fake.addOnCloseMutex.Unlock()
	fake.AddOnCloseStub = stub
}

func (fake *FakePublishedTrack) AddOnCloseArgsForCall(i int) func() {
	fake.addOnCloseMutex.RLock()
	defer fake.addOnCloseMutex.RUnlock()
	argsForCall := fake.addOnCloseArgsForCall[i]
	return argsForCall.arg1
}

func (fake *FakePublishedTrack) AddSubscriber(arg1 types.Participant) error {
	fake.addSubscriberMutex.Lock()
	ret, specificReturn := fake.addSubscriberReturnsOnCall[len(fake.addSubscriberArgsForCall)]
	fake.addSubscriberArgsForCall = append(fake.addSubscriberArgsForCall, struct {
		arg1 types.Participant
	}{arg1})
	stub := fake.AddSubscriberStub
	fakeReturns := fake.addSubscriberReturns
	fake.recordInvocation("AddSubscriber", []interface{}{arg1})
	fake.addSubscriberMutex.Unlock()
	if stub != nil {
		return stub(arg1)
	}
	if specificReturn {
		return ret.result1
	}
	return fakeReturns.result1
}

func (fake *FakePublishedTrack) AddSubscriberCallCount() int {
	fake.addSubscriberMutex.RLock()
	defer fake.addSubscriberMutex.RUnlock()
	return len(fake.addSubscriberArgsForCall)
}

func (fake *FakePublishedTrack) AddSubscriberCalls(stub func(types.Participant) error) {
	fake.addSubscriberMutex.Lock()
	defer fake.addSubscriberMutex.Unlock()
	fake.AddSubscriberStub = stub
}

func (fake *FakePublishedTrack) AddSubscriberArgsForCall(i int) types.Participant {
	fake.addSubscriberMutex.RLock()
	defer fake.addSubscriberMutex.RUnlock()
	argsForCall := fake.addSubscriberArgsForCall[i]
	return argsForCall.arg1
}

func (fake *FakePublishedTrack) AddSubscriberReturns(result1 error) {
	fake.addSubscriberMutex.Lock()
	defer fake.addSubscriberMutex.Unlock()
	fake.AddSubscriberStub = nil
	fake.addSubscriberReturns = struct {
		result1 error
	}{result1}
}

func (fake *FakePublishedTrack) AddSubscriberReturnsOnCall(i int, result1 error) {
	fake.addSubscriberMutex.Lock()
	defer fake.addSubscriberMutex.Unlock()
	fake.AddSubscriberStub = nil
	if fake.addSubscriberReturnsOnCall == nil {
		fake.addSubscriberReturnsOnCall = make(map[int]struct {
			result1 error
		})
	}
	fake.addSubscriberReturnsOnCall[i] = struct {
		result1 error
	}{result1}
}

func (fake *FakePublishedTrack) GetQualityForDimension(arg1 uint32, arg2 uint32) livekit.VideoQuality {
	fake.getQualityForDimensionMutex.Lock()
	ret, specificReturn := fake.getQualityForDimensionReturnsOnCall[len(fake.getQualityForDimensionArgsForCall)]
	fake.getQualityForDimensionArgsForCall = append(fake.getQualityForDimensionArgsForCall, struct {
		arg1 uint32
		arg2 uint32
	}{arg1, arg2})
	stub := fake.GetQualityForDimensionStub
	fakeReturns := fake.getQualityForDimensionReturns
	fake.recordInvocation("GetQualityForDimension", []interface{}{arg1, arg2})
	fake.getQualityForDimensionMutex.Unlock()
	if stub != nil {
		return stub(arg1, arg2)
	}
	if specificReturn {
		return ret.result1
	}
	return fakeReturns.result1
}

func (fake *FakePublishedTrack) GetQualityForDimensionCallCount() int {
	fake.getQualityForDimensionMutex.RLock()
	defer fake.getQualityForDimensionMutex.RUnlock()
	return len(fake.getQualityForDimensionArgsForCall)
}

func (fake *FakePublishedTrack) GetQualityForDimensionCalls(stub func(uint32, uint32) livekit.VideoQuality) {
	fake.getQualityForDimensionMutex.Lock()
	defer fake.getQualityForDimensionMutex.Unlock()
	fake.GetQualityForDimensionStub = stub
}

func (fake *FakePublishedTrack) GetQualityForDimensionArgsForCall(i int) (uint32, uint32) {
	fake.getQualityForDimensionMutex.RLock()
	defer fake.getQualityForDimensionMutex.RUnlock()
	argsForCall := fake.getQualityForDimensionArgsForCall[i]
	return argsForCall.arg1, argsForCall.arg2
}

func (fake *FakePublishedTrack) GetQualityForDimensionReturns(result1 livekit.VideoQuality) {
	fake.getQualityForDimensionMutex.Lock()
	defer fake.getQualityForDimensionMutex.Unlock()
	fake.GetQualityForDimensionStub = nil
	fake.getQualityForDimensionReturns = struct {
		result1 livekit.VideoQuality
	}{result1}
}

func (fake *FakePublishedTrack) GetQualityForDimensionReturnsOnCall(i int, result1 livekit.VideoQuality) {
	fake.getQualityForDimensionMutex.Lock()
	defer fake.getQualityForDimensionMutex.Unlock()
	fake.GetQualityForDimensionStub = nil
	if fake.getQualityForDimensionReturnsOnCall == nil {
		fake.getQualityForDimensionReturnsOnCall = make(map[int]struct {
			result1 livekit.VideoQuality
		})
	}
	fake.getQualityForDimensionReturnsOnCall[i] = struct {
		result1 livekit.VideoQuality
	}{result1}
}

func (fake *FakePublishedTrack) ID() string {
	fake.iDMutex.Lock()
	ret, specificReturn := fake.iDReturnsOnCall[len(fake.iDArgsForCall)]
	fake.iDArgsForCall = append(fake.iDArgsForCall, struct {
	}{})
	stub := fake.IDStub
	fakeReturns := fake.iDReturns
	fake.recordInvocation("ID", []interface{}{})
	fake.iDMutex.Unlock()
	if stub != nil {
		return stub()
	}
	if specificReturn {
		return ret.result1
	}
	return fakeReturns.result1
}

func (fake *FakePublishedTrack) IDCallCount() int {
	fake.iDMutex.RLock()
	defer fake.iDMutex.RUnlock()
	return len(fake.iDArgsForCall)
}

func (fake *FakePublishedTrack) IDCalls(stub func() string) {
	fake.iDMutex.Lock()
	defer fake.iDMutex.Unlock()
	fake.IDStub = stub
}

func (fake *FakePublishedTrack) IDReturns(result1 string) {
	fake.iDMutex.Lock()
	defer fake.iDMutex.Unlock()
	fake.IDStub = nil
	fake.iDReturns = struct {
		result1 string
	}{result1}
}

func (fake *FakePublishedTrack) IDReturnsOnCall(i int, result1 string) {
	fake.iDMutex.Lock()
	defer fake.iDMutex.Unlock()
	fake.IDStub = nil
	if fake.iDReturnsOnCall == nil {
		fake.iDReturnsOnCall = make(map[int]struct {
			result1 string
		})
	}
	fake.iDReturnsOnCall[i] = struct {
		result1 string
	}{result1}
}

func (fake *FakePublishedTrack) IsMuted() bool {
	fake.isMutedMutex.Lock()
	ret, specificReturn := fake.isMutedReturnsOnCall[len(fake.isMutedArgsForCall)]
	fake.isMutedArgsForCall = append(fake.isMutedArgsForCall, struct {
	}{})
	stub := fake.IsMutedStub
	fakeReturns := fake.isMutedReturns
	fake.recordInvocation("IsMuted", []interface{}{})
	fake.isMutedMutex.Unlock()
	if stub != nil {
		return stub()
	}
	if specificReturn {
		return ret.result1
	}
	return fakeReturns.result1
}

func (fake *FakePublishedTrack) IsMutedCallCount() int {
	fake.isMutedMutex.RLock()
	defer fake.isMutedMutex.RUnlock()
	return len(fake.isMutedArgsForCall)
}

func (fake *FakePublishedTrack) IsMutedCalls(stub func() bool) {
	fake.isMutedMutex.Lock()
	defer fake.isMutedMutex.Unlock()
	fake.IsMutedStub = stub
}

func (fake *FakePublishedTrack) IsMutedReturns(result1 bool) {
	fake.isMutedMutex.Lock()
	defer fake.isMutedMutex.Unlock()
	fake.IsMutedStub = nil
	fake.isMutedReturns = struct {
		result1 bool
	}{result1}
}

func (fake *FakePublishedTrack) IsMutedReturnsOnCall(i int, result1 bool) {
	fake.isMutedMutex.Lock()
	defer fake.isMutedMutex.Unlock()
	fake.IsMutedStub = nil
	if fake.isMutedReturnsOnCall == nil {
		fake.isMutedReturnsOnCall = make(map[int]struct {
			result1 bool
		})
	}
	fake.isMutedReturnsOnCall[i] = struct {
		result1 bool
	}{result1}
}

func (fake *FakePublishedTrack) IsSimulcast() bool {
	fake.isSimulcastMutex.Lock()
	ret, specificReturn := fake.isSimulcastReturnsOnCall[len(fake.isSimulcastArgsForCall)]
	fake.isSimulcastArgsForCall = append(fake.isSimulcastArgsForCall, struct {
	}{})
	stub := fake.IsSimulcastStub
	fakeReturns := fake.isSimulcastReturns
	fake.recordInvocation("IsSimulcast", []interface{}{})
	fake.isSimulcastMutex.Unlock()
	if stub != nil {
		return stub()
	}
	if specificReturn {
		return ret.result1
	}
	return fakeReturns.result1
}

func (fake *FakePublishedTrack) IsSimulcastCallCount() int {
	fake.isSimulcastMutex.RLock()
	defer fake.isSimulcastMutex.RUnlock()
	return len(fake.isSimulcastArgsForCall)
}

func (fake *FakePublishedTrack) IsSimulcastCalls(stub func() bool) {
	fake.isSimulcastMutex.Lock()
	defer fake.isSimulcastMutex.Unlock()
	fake.IsSimulcastStub = stub
}

func (fake *FakePublishedTrack) IsSimulcastReturns(result1 bool) {
	fake.isSimulcastMutex.Lock()
	defer fake.isSimulcastMutex.Unlock()
	fake.IsSimulcastStub = nil
	fake.isSimulcastReturns = struct {
		result1 bool
	}{result1}
}

func (fake *FakePublishedTrack) IsSimulcastReturnsOnCall(i int, result1 bool) {
	fake.isSimulcastMutex.Lock()
	defer fake.isSimulcastMutex.Unlock()
	fake.IsSimulcastStub = nil
	if fake.isSimulcastReturnsOnCall == nil {
		fake.isSimulcastReturnsOnCall = make(map[int]struct {
			result1 bool
		})
	}
	fake.isSimulcastReturnsOnCall[i] = struct {
		result1 bool
	}{result1}
}

func (fake *FakePublishedTrack) IsSubscriber(arg1 string) bool {
	fake.isSubscriberMutex.Lock()
	ret, specificReturn := fake.isSubscriberReturnsOnCall[len(fake.isSubscriberArgsForCall)]
	fake.isSubscriberArgsForCall = append(fake.isSubscriberArgsForCall, struct {
		arg1 string
	}{arg1})
	stub := fake.IsSubscriberStub
	fakeReturns := fake.isSubscriberReturns
	fake.recordInvocation("IsSubscriber", []interface{}{arg1})
	fake.isSubscriberMutex.Unlock()
	if stub != nil {
		return stub(arg1)
	}
	if specificReturn {
		return ret.result1
	}
	return fakeReturns.result1
}

func (fake *FakePublishedTrack) IsSubscriberCallCount() int {
	fake.isSubscriberMutex.RLock()
	defer fake.isSubscriberMutex.RUnlock()
	return len(fake.isSubscriberArgsForCall)
}

func (fake *FakePublishedTrack) IsSubscriberCalls(stub func(string) bool) {
	fake.isSubscriberMutex.Lock()
	defer fake.isSubscriberMutex.Unlock()
	fake.IsSubscriberStub = stub
}

func (fake *FakePublishedTrack) IsSubscriberArgsForCall(i int) string {
	fake.isSubscriberMutex.RLock()
	defer fake.isSubscriberMutex.RUnlock()
	argsForCall := fake.isSubscriberArgsForCall[i]
	return argsForCall.arg1
}

func (fake *FakePublishedTrack) IsSubscriberReturns(result1 bool) {
	fake.isSubscriberMutex.Lock()
	defer fake.isSubscriberMutex.Unlock()
	fake.IsSubscriberStub = nil
	fake.isSubscriberReturns = struct {
		result1 bool
	}{result1}
}

func (fake *FakePublishedTrack) IsSubscriberReturnsOnCall(i int, result1 bool) {
	fake.isSubscriberMutex.Lock()
	defer fake.isSubscriberMutex.Unlock()
	fake.IsSubscriberStub = nil
	if fake.isSubscriberReturnsOnCall == nil {
		fake.isSubscriberReturnsOnCall = make(map[int]struct {
			result1 bool
		})
	}
	fake.isSubscriberReturnsOnCall[i] = struct {
		result1 bool
	}{result1}
}

func (fake *FakePublishedTrack) Kind() livekit.TrackType {
	fake.kindMutex.Lock()
	ret, specificReturn := fake.kindReturnsOnCall[len(fake.kindArgsForCall)]
	fake.kindArgsForCall = append(fake.kindArgsForCall, struct {
	}{})
	stub := fake.KindStub
	fakeReturns := fake.kindReturns
	fake.recordInvocation("Kind", []interface{}{})
	fake.kindMutex.Unlock()
	if stub != nil {
		return stub()
	}
	if specificReturn {
		return ret.result1
	}
	return fakeReturns.result1
}

func (fake *FakePublishedTrack) KindCallCount() int {
	fake.kindMutex.RLock()
	defer fake.kindMutex.RUnlock()
	return len(fake.kindArgsForCall)
}

func (fake *FakePublishedTrack) KindCalls(stub func() livekit.TrackType) {
	fake.kindMutex.Lock()
	defer fake.kindMutex.Unlock()
	fake.KindStub = stub
}

func (fake *FakePublishedTrack) KindReturns(result1 livekit.TrackType) {
	fake.kindMutex.Lock()
	defer fake.kindMutex.Unlock()
	fake.KindStub = nil
	fake.kindReturns = struct {
		result1 livekit.TrackType
	}{result1}
}

func (fake *FakePublishedTrack) KindReturnsOnCall(i int, result1 livekit.TrackType) {
	fake.kindMutex.Lock()
	defer fake.kindMutex.Unlock()
	fake.KindStub = nil
	if fake.kindReturnsOnCall == nil {
		fake.kindReturnsOnCall = make(map[int]struct {
			result1 livekit.TrackType
		})
	}
	fake.kindReturnsOnCall[i] = struct {
		result1 livekit.TrackType
	}{result1}
}

func (fake *FakePublishedTrack) Name() string {
	fake.nameMutex.Lock()
	ret, specificReturn := fake.nameReturnsOnCall[len(fake.nameArgsForCall)]
	fake.nameArgsForCall = append(fake.nameArgsForCall, struct {
	}{})
	stub := fake.NameStub
	fakeReturns := fake.nameReturns
	fake.recordInvocation("Name", []interface{}{})
	fake.nameMutex.Unlock()
	if stub != nil {
		return stub()
	}
	if specificReturn {
		return ret.result1
	}
	return fakeReturns.result1
}

func (fake *FakePublishedTrack) NameCallCount() int {
	fake.nameMutex.RLock()
	defer fake.nameMutex.RUnlock()
	return len(fake.nameArgsForCall)
}

func (fake *FakePublishedTrack) NameCalls(stub func() string) {
	fake.nameMutex.Lock()
	defer fake.nameMutex.Unlock()
	fake.NameStub = stub
}

func (fake *FakePublishedTrack) NameReturns(result1 string) {
	fake.nameMutex.Lock()
	defer fake.nameMutex.Unlock()
	fake.NameStub = nil
	fake.nameReturns = struct {
		result1 string
	}{result1}
}

func (fake *FakePublishedTrack) NameReturnsOnCall(i int, result1 string) {
	fake.nameMutex.Lock()
	defer fake.nameMutex.Unlock()
	fake.NameStub = nil
	if fake.nameReturnsOnCall == nil {
		fake.nameReturnsOnCall = make(map[int]struct {
			result1 string
		})
	}
	fake.nameReturnsOnCall[i] = struct {
		result1 string
	}{result1}
}

func (fake *FakePublishedTrack) NotifySubscriberMaxQuality(arg1 string, arg2 livekit.VideoQuality) {
	fake.notifySubscriberMaxQualityMutex.Lock()
	fake.notifySubscriberMaxQualityArgsForCall = append(fake.notifySubscriberMaxQualityArgsForCall, struct {
		arg1 string
		arg2 livekit.VideoQuality
	}{arg1, arg2})
	stub := fake.NotifySubscriberMaxQualityStub
	fake.recordInvocation("NotifySubscriberMaxQuality", []interface{}{arg1, arg2})
	fake.notifySubscriberMaxQualityMutex.Unlock()
	if stub != nil {
		fake.NotifySubscriberMaxQualityStub(arg1, arg2)
	}
}

func (fake *FakePublishedTrack) NotifySubscriberMaxQualityCallCount() int {
	fake.notifySubscriberMaxQualityMutex.RLock()
	defer fake.notifySubscriberMaxQualityMutex.RUnlock()
	return len(fake.notifySubscriberMaxQualityArgsForCall)
}

func (fake *FakePublishedTrack) NotifySubscriberMaxQualityCalls(stub func(string, livekit.VideoQuality)) {
	fake.notifySubscriberMaxQualityMutex.Lock()
	defer fake.notifySubscriberMaxQualityMutex.Unlock()
	fake.NotifySubscriberMaxQualityStub = stub
}

func (fake *FakePublishedTrack) NotifySubscriberMaxQualityArgsForCall(i int) (string, livekit.VideoQuality) {
	fake.notifySubscriberMaxQualityMutex.RLock()
	defer fake.notifySubscriberMaxQualityMutex.RUnlock()
	argsForCall := fake.notifySubscriberMaxQualityArgsForCall[i]
	return argsForCall.arg1, argsForCall.arg2
}

func (fake *FakePublishedTrack) NotifySubscriberMute(arg1 string) {
	fake.notifySubscriberMuteMutex.Lock()
	fake.notifySubscriberMuteArgsForCall = append(fake.notifySubscriberMuteArgsForCall, struct {
		arg1 string
	}{arg1})
	stub := fake.NotifySubscriberMuteStub
	fake.recordInvocation("NotifySubscriberMute", []interface{}{arg1})
	fake.notifySubscriberMuteMutex.Unlock()
	if stub != nil {
		fake.NotifySubscriberMuteStub(arg1)
	}
}

func (fake *FakePublishedTrack) NotifySubscriberMuteCallCount() int {
	fake.notifySubscriberMuteMutex.RLock()
	defer fake.notifySubscriberMuteMutex.RUnlock()
	return len(fake.notifySubscriberMuteArgsForCall)
}

func (fake *FakePublishedTrack) NotifySubscriberMuteCalls(stub func(string)) {
	fake.notifySubscriberMuteMutex.Lock()
	defer fake.notifySubscriberMuteMutex.Unlock()
	fake.NotifySubscriberMuteStub = stub
}

func (fake *FakePublishedTrack) NotifySubscriberMuteArgsForCall(i int) string {
	fake.notifySubscriberMuteMutex.RLock()
	defer fake.notifySubscriberMuteMutex.RUnlock()
	argsForCall := fake.notifySubscriberMuteArgsForCall[i]
	return argsForCall.arg1
}

func (fake *FakePublishedTrack) NumUpTracks() (uint32, uint32) {
	fake.numUpTracksMutex.Lock()
	ret, specificReturn := fake.numUpTracksReturnsOnCall[len(fake.numUpTracksArgsForCall)]
	fake.numUpTracksArgsForCall = append(fake.numUpTracksArgsForCall, struct {
	}{})
	stub := fake.NumUpTracksStub
	fakeReturns := fake.numUpTracksReturns
	fake.recordInvocation("NumUpTracks", []interface{}{})
	fake.numUpTracksMutex.Unlock()
	if stub != nil {
		return stub()
	}
	if specificReturn {
		return ret.result1, ret.result2
	}
	return fakeReturns.result1, fakeReturns.result2
}

func (fake *FakePublishedTrack) NumUpTracksCallCount() int {
	fake.numUpTracksMutex.RLock()
	defer fake.numUpTracksMutex.RUnlock()
	return len(fake.numUpTracksArgsForCall)
}

func (fake *FakePublishedTrack) NumUpTracksCalls(stub func() (uint32, uint32)) {
	fake.numUpTracksMutex.Lock()
	defer fake.numUpTracksMutex.Unlock()
	fake.NumUpTracksStub = stub
}

func (fake *FakePublishedTrack) NumUpTracksReturns(result1 uint32, result2 uint32) {
	fake.numUpTracksMutex.Lock()
	defer fake.numUpTracksMutex.Unlock()
	fake.NumUpTracksStub = nil
	fake.numUpTracksReturns = struct {
		result1 uint32
		result2 uint32
	}{result1, result2}
}

func (fake *FakePublishedTrack) NumUpTracksReturnsOnCall(i int, result1 uint32, result2 uint32) {
	fake.numUpTracksMutex.Lock()
	defer fake.numUpTracksMutex.Unlock()
	fake.NumUpTracksStub = nil
	if fake.numUpTracksReturnsOnCall == nil {
		fake.numUpTracksReturnsOnCall = make(map[int]struct {
			result1 uint32
			result2 uint32
		})
	}
	fake.numUpTracksReturnsOnCall[i] = struct {
		result1 uint32
		result2 uint32
	}{result1, result2}
}

func (fake *FakePublishedTrack) OnSubscribedMaxQualityChange(arg1 func(trackSid string, subscribedQualities []*livekit.SubscribedQuality) error) {
	fake.onSubscribedMaxQualityChangeMutex.Lock()
	fake.onSubscribedMaxQualityChangeArgsForCall = append(fake.onSubscribedMaxQualityChangeArgsForCall, struct {
		arg1 func(trackSid string, subscribedQualities []*livekit.SubscribedQuality) error
	}{arg1})
	stub := fake.OnSubscribedMaxQualityChangeStub
	fake.recordInvocation("OnSubscribedMaxQualityChange", []interface{}{arg1})
	fake.onSubscribedMaxQualityChangeMutex.Unlock()
	if stub != nil {
		fake.OnSubscribedMaxQualityChangeStub(arg1)
	}
}

func (fake *FakePublishedTrack) OnSubscribedMaxQualityChangeCallCount() int {
	fake.onSubscribedMaxQualityChangeMutex.RLock()
	defer fake.onSubscribedMaxQualityChangeMutex.RUnlock()
	return len(fake.onSubscribedMaxQualityChangeArgsForCall)
}

func (fake *FakePublishedTrack) OnSubscribedMaxQualityChangeCalls(stub func(func(trackSid string, subscribedQualities []*livekit.SubscribedQuality) error)) {
	fake.onSubscribedMaxQualityChangeMutex.Lock()
	defer fake.onSubscribedMaxQualityChangeMutex.Unlock()
	fake.OnSubscribedMaxQualityChangeStub = stub
}

func (fake *FakePublishedTrack) OnSubscribedMaxQualityChangeArgsForCall(i int) func(trackSid string, subscribedQualities []*livekit.SubscribedQuality) error {
	fake.onSubscribedMaxQualityChangeMutex.RLock()
	defer fake.onSubscribedMaxQualityChangeMutex.RUnlock()
	argsForCall := fake.onSubscribedMaxQualityChangeArgsForCall[i]
	return argsForCall.arg1
}

func (fake *FakePublishedTrack) PublishLossPercentage() uint32 {
	fake.publishLossPercentageMutex.Lock()
	ret, specificReturn := fake.publishLossPercentageReturnsOnCall[len(fake.publishLossPercentageArgsForCall)]
	fake.publishLossPercentageArgsForCall = append(fake.publishLossPercentageArgsForCall, struct {
	}{})
	stub := fake.PublishLossPercentageStub
	fakeReturns := fake.publishLossPercentageReturns
	fake.recordInvocation("PublishLossPercentage", []interface{}{})
	fake.publishLossPercentageMutex.Unlock()
	if stub != nil {
		return stub()
	}
	if specificReturn {
		return ret.result1
	}
	return fakeReturns.result1
}

func (fake *FakePublishedTrack) PublishLossPercentageCallCount() int {
	fake.publishLossPercentageMutex.RLock()
	defer fake.publishLossPercentageMutex.RUnlock()
	return len(fake.publishLossPercentageArgsForCall)
}

func (fake *FakePublishedTrack) PublishLossPercentageCalls(stub func() uint32) {
	fake.publishLossPercentageMutex.Lock()
	defer fake.publishLossPercentageMutex.Unlock()
	fake.PublishLossPercentageStub = stub
}

func (fake *FakePublishedTrack) PublishLossPercentageReturns(result1 uint32) {
	fake.publishLossPercentageMutex.Lock()
	defer fake.publishLossPercentageMutex.Unlock()
	fake.PublishLossPercentageStub = nil
	fake.publishLossPercentageReturns = struct {
		result1 uint32
	}{result1}
}

func (fake *FakePublishedTrack) PublishLossPercentageReturnsOnCall(i int, result1 uint32) {
	fake.publishLossPercentageMutex.Lock()
	defer fake.publishLossPercentageMutex.Unlock()
	fake.PublishLossPercentageStub = nil
	if fake.publishLossPercentageReturnsOnCall == nil {
		fake.publishLossPercentageReturnsOnCall = make(map[int]struct {
			result1 uint32
		})
	}
	fake.publishLossPercentageReturnsOnCall[i] = struct {
		result1 uint32
	}{result1}
}

func (fake *FakePublishedTrack) Receiver() sfu.TrackReceiver {
	fake.receiverMutex.Lock()
	ret, specificReturn := fake.receiverReturnsOnCall[len(fake.receiverArgsForCall)]
	fake.receiverArgsForCall = append(fake.receiverArgsForCall, struct {
	}{})
	stub := fake.ReceiverStub
	fakeReturns := fake.receiverReturns
	fake.recordInvocation("Receiver", []interface{}{})
	fake.receiverMutex.Unlock()
	if stub != nil {
		return stub()
	}
	if specificReturn {
		return ret.result1
	}
	return fakeReturns.result1
}

func (fake *FakePublishedTrack) ReceiverCallCount() int {
	fake.receiverMutex.RLock()
	defer fake.receiverMutex.RUnlock()
	return len(fake.receiverArgsForCall)
}

func (fake *FakePublishedTrack) ReceiverCalls(stub func() sfu.TrackReceiver) {
	fake.receiverMutex.Lock()
	defer fake.receiverMutex.Unlock()
	fake.ReceiverStub = stub
}

func (fake *FakePublishedTrack) ReceiverReturns(result1 sfu.TrackReceiver) {
	fake.receiverMutex.Lock()
	defer fake.receiverMutex.Unlock()
	fake.ReceiverStub = nil
	fake.receiverReturns = struct {
		result1 sfu.TrackReceiver
	}{result1}
}

func (fake *FakePublishedTrack) ReceiverReturnsOnCall(i int, result1 sfu.TrackReceiver) {
	fake.receiverMutex.Lock()
	defer fake.receiverMutex.Unlock()
	fake.ReceiverStub = nil
	if fake.receiverReturnsOnCall == nil {
		fake.receiverReturnsOnCall = make(map[int]struct {
			result1 sfu.TrackReceiver
		})
	}
	fake.receiverReturnsOnCall[i] = struct {
		result1 sfu.TrackReceiver
	}{result1}
}

func (fake *FakePublishedTrack) RemoveAllSubscribers() {
	fake.removeAllSubscribersMutex.Lock()
	fake.removeAllSubscribersArgsForCall = append(fake.removeAllSubscribersArgsForCall, struct {
	}{})
	stub := fake.RemoveAllSubscribersStub
	fake.recordInvocation("RemoveAllSubscribers", []interface{}{})
	fake.removeAllSubscribersMutex.Unlock()
	if stub != nil {
		fake.RemoveAllSubscribersStub()
	}
}

func (fake *FakePublishedTrack) RemoveAllSubscribersCallCount() int {
	fake.removeAllSubscribersMutex.RLock()
	defer fake.removeAllSubscribersMutex.RUnlock()
	return len(fake.removeAllSubscribersArgsForCall)
}

func (fake *FakePublishedTrack) RemoveAllSubscribersCalls(stub func()) {
	fake.removeAllSubscribersMutex.Lock()
	defer fake.removeAllSubscribersMutex.Unlock()
	fake.RemoveAllSubscribersStub = stub
}

func (fake *FakePublishedTrack) RemoveSubscriber(arg1 string) {
	fake.removeSubscriberMutex.Lock()
	fake.removeSubscriberArgsForCall = append(fake.removeSubscriberArgsForCall, struct {
		arg1 string
	}{arg1})
	stub := fake.RemoveSubscriberStub
	fake.recordInvocation("RemoveSubscriber", []interface{}{arg1})
	fake.removeSubscriberMutex.Unlock()
	if stub != nil {
		fake.RemoveSubscriberStub(arg1)
	}
}

func (fake *FakePublishedTrack) RemoveSubscriberCallCount() int {
	fake.removeSubscriberMutex.RLock()
	defer fake.removeSubscriberMutex.RUnlock()
	return len(fake.removeSubscriberArgsForCall)
}

func (fake *FakePublishedTrack) RemoveSubscriberCalls(stub func(string)) {
	fake.removeSubscriberMutex.Lock()
	defer fake.removeSubscriberMutex.Unlock()
	fake.RemoveSubscriberStub = stub
}

func (fake *FakePublishedTrack) RemoveSubscriberArgsForCall(i int) string {
	fake.removeSubscriberMutex.RLock()
	defer fake.removeSubscriberMutex.RUnlock()
	argsForCall := fake.removeSubscriberArgsForCall[i]
	return argsForCall.arg1
}

func (fake *FakePublishedTrack) SdpCid() string {
	fake.sdpCidMutex.Lock()
	ret, specificReturn := fake.sdpCidReturnsOnCall[len(fake.sdpCidArgsForCall)]
	fake.sdpCidArgsForCall = append(fake.sdpCidArgsForCall, struct {
	}{})
	stub := fake.SdpCidStub
	fakeReturns := fake.sdpCidReturns
	fake.recordInvocation("SdpCid", []interface{}{})
	fake.sdpCidMutex.Unlock()
	if stub != nil {
		return stub()
	}
	if specificReturn {
		return ret.result1
	}
	return fakeReturns.result1
}

func (fake *FakePublishedTrack) SdpCidCallCount() int {
	fake.sdpCidMutex.RLock()
	defer fake.sdpCidMutex.RUnlock()
	return len(fake.sdpCidArgsForCall)
}

func (fake *FakePublishedTrack) SdpCidCalls(stub func() string) {
	fake.sdpCidMutex.Lock()
	defer fake.sdpCidMutex.Unlock()
	fake.SdpCidStub = stub
}

func (fake *FakePublishedTrack) SdpCidReturns(result1 string) {
	fake.sdpCidMutex.Lock()
	defer fake.sdpCidMutex.Unlock()
	fake.SdpCidStub = nil
	fake.sdpCidReturns = struct {
		result1 string
	}{result1}
}

func (fake *FakePublishedTrack) SdpCidReturnsOnCall(i int, result1 string) {
	fake.sdpCidMutex.Lock()
	defer fake.sdpCidMutex.Unlock()
	fake.SdpCidStub = nil
	if fake.sdpCidReturnsOnCall == nil {
		fake.sdpCidReturnsOnCall = make(map[int]struct {
			result1 string
		})
	}
	fake.sdpCidReturnsOnCall[i] = struct {
		result1 string
	}{result1}
}

func (fake *FakePublishedTrack) SetMuted(arg1 bool) {
	fake.setMutedMutex.Lock()
	fake.setMutedArgsForCall = append(fake.setMutedArgsForCall, struct {
		arg1 bool
	}{arg1})
	stub := fake.SetMutedStub
	fake.recordInvocation("SetMuted", []interface{}{arg1})
	fake.setMutedMutex.Unlock()
	if stub != nil {
		fake.SetMutedStub(arg1)
	}
}

func (fake *FakePublishedTrack) SetMutedCallCount() int {
	fake.setMutedMutex.RLock()
	defer fake.setMutedMutex.RUnlock()
	return len(fake.setMutedArgsForCall)
}

func (fake *FakePublishedTrack) SetMutedCalls(stub func(bool)) {
	fake.setMutedMutex.Lock()
	defer fake.setMutedMutex.Unlock()
	fake.SetMutedStub = stub
}

func (fake *FakePublishedTrack) SetMutedArgsForCall(i int) bool {
	fake.setMutedMutex.RLock()
	defer fake.setMutedMutex.RUnlock()
	argsForCall := fake.setMutedArgsForCall[i]
	return argsForCall.arg1
}

func (fake *FakePublishedTrack) SignalCid() string {
	fake.signalCidMutex.Lock()
	ret, specificReturn := fake.signalCidReturnsOnCall[len(fake.signalCidArgsForCall)]
	fake.signalCidArgsForCall = append(fake.signalCidArgsForCall, struct {
	}{})
	stub := fake.SignalCidStub
	fakeReturns := fake.signalCidReturns
	fake.recordInvocation("SignalCid", []interface{}{})
	fake.signalCidMutex.Unlock()
	if stub != nil {
		return stub()
	}
	if specificReturn {
		return ret.result1
	}
	return fakeReturns.result1
}

func (fake *FakePublishedTrack) SignalCidCallCount() int {
	fake.signalCidMutex.RLock()
	defer fake.signalCidMutex.RUnlock()
	return len(fake.signalCidArgsForCall)
}

func (fake *FakePublishedTrack) SignalCidCalls(stub func() string) {
	fake.signalCidMutex.Lock()
	defer fake.signalCidMutex.Unlock()
	fake.SignalCidStub = stub
}

func (fake *FakePublishedTrack) SignalCidReturns(result1 string) {
	fake.signalCidMutex.Lock()
	defer fake.signalCidMutex.Unlock()
	fake.SignalCidStub = nil
	fake.signalCidReturns = struct {
		result1 string
	}{result1}
}

func (fake *FakePublishedTrack) SignalCidReturnsOnCall(i int, result1 string) {
	fake.signalCidMutex.Lock()
	defer fake.signalCidMutex.Unlock()
	fake.SignalCidStub = nil
	if fake.signalCidReturnsOnCall == nil {
		fake.signalCidReturnsOnCall = make(map[int]struct {
			result1 string
		})
	}
	fake.signalCidReturnsOnCall[i] = struct {
		result1 string
	}{result1}
}

func (fake *FakePublishedTrack) Source() livekit.TrackSource {
	fake.sourceMutex.Lock()
	ret, specificReturn := fake.sourceReturnsOnCall[len(fake.sourceArgsForCall)]
	fake.sourceArgsForCall = append(fake.sourceArgsForCall, struct {
	}{})
	stub := fake.SourceStub
	fakeReturns := fake.sourceReturns
	fake.recordInvocation("Source", []interface{}{})
	fake.sourceMutex.Unlock()
	if stub != nil {
		return stub()
	}
	if specificReturn {
		return ret.result1
	}
	return fakeReturns.result1
}

func (fake *FakePublishedTrack) SourceCallCount() int {
	fake.sourceMutex.RLock()
	defer fake.sourceMutex.RUnlock()
	return len(fake.sourceArgsForCall)
}

func (fake *FakePublishedTrack) SourceCalls(stub func() livekit.TrackSource) {
	fake.sourceMutex.Lock()
	defer fake.sourceMutex.Unlock()
	fake.SourceStub = stub
}

func (fake *FakePublishedTrack) SourceReturns(result1 livekit.TrackSource) {
	fake.sourceMutex.Lock()
	defer fake.sourceMutex.Unlock()
	fake.SourceStub = nil
	fake.sourceReturns = struct {
		result1 livekit.TrackSource
	}{result1}
}

func (fake *FakePublishedTrack) SourceReturnsOnCall(i int, result1 livekit.TrackSource) {
	fake.sourceMutex.Lock()
	defer fake.sourceMutex.Unlock()
	fake.SourceStub = nil
	if fake.sourceReturnsOnCall == nil {
		fake.sourceReturnsOnCall = make(map[int]struct {
			result1 livekit.TrackSource
		})
	}
	fake.sourceReturnsOnCall[i] = struct {
		result1 livekit.TrackSource
	}{result1}
}

func (fake *FakePublishedTrack) ToProto() *livekit.TrackInfo {
	fake.toProtoMutex.Lock()
	ret, specificReturn := fake.toProtoReturnsOnCall[len(fake.toProtoArgsForCall)]
	fake.toProtoArgsForCall = append(fake.toProtoArgsForCall, struct {
	}{})
	stub := fake.ToProtoStub
	fakeReturns := fake.toProtoReturns
	fake.recordInvocation("ToProto", []interface{}{})
	fake.toProtoMutex.Unlock()
	if stub != nil {
		return stub()
	}
	if specificReturn {
		return ret.result1
	}
	return fakeReturns.result1
}

func (fake *FakePublishedTrack) ToProtoCallCount() int {
	fake.toProtoMutex.RLock()
	defer fake.toProtoMutex.RUnlock()
	return len(fake.toProtoArgsForCall)
}

func (fake *FakePublishedTrack) ToProtoCalls(stub func() *livekit.TrackInfo) {
	fake.toProtoMutex.Lock()
	defer fake.toProtoMutex.Unlock()
	fake.ToProtoStub = stub
}

func (fake *FakePublishedTrack) ToProtoReturns(result1 *livekit.TrackInfo) {
	fake.toProtoMutex.Lock()
	defer fake.toProtoMutex.Unlock()
	fake.ToProtoStub = nil
	fake.toProtoReturns = struct {
		result1 *livekit.TrackInfo
	}{result1}
}

func (fake *FakePublishedTrack) ToProtoReturnsOnCall(i int, result1 *livekit.TrackInfo) {
	fake.toProtoMutex.Lock()
	defer fake.toProtoMutex.Unlock()
	fake.ToProtoStub = nil
	if fake.toProtoReturnsOnCall == nil {
		fake.toProtoReturnsOnCall = make(map[int]struct {
			result1 *livekit.TrackInfo
		})
	}
	fake.toProtoReturnsOnCall[i] = struct {
		result1 *livekit.TrackInfo
	}{result1}
}

func (fake *FakePublishedTrack) UpdateVideoLayers(arg1 []*livekit.VideoLayer) {
	var arg1Copy []*livekit.VideoLayer
	if arg1 != nil {
		arg1Copy = make([]*livekit.VideoLayer, len(arg1))
		copy(arg1Copy, arg1)
	}
	fake.updateVideoLayersMutex.Lock()
	fake.updateVideoLayersArgsForCall = append(fake.updateVideoLayersArgsForCall, struct {
		arg1 []*livekit.VideoLayer
	}{arg1Copy})
	stub := fake.UpdateVideoLayersStub
	fake.recordInvocation("UpdateVideoLayers", []interface{}{arg1Copy})
	fake.updateVideoLayersMutex.Unlock()
	if stub != nil {
		fake.UpdateVideoLayersStub(arg1)
	}
}

func (fake *FakePublishedTrack) UpdateVideoLayersCallCount() int {
	fake.updateVideoLayersMutex.RLock()
	defer fake.updateVideoLayersMutex.RUnlock()
	return len(fake.updateVideoLayersArgsForCall)
}

func (fake *FakePublishedTrack) UpdateVideoLayersCalls(stub func([]*livekit.VideoLayer)) {
	fake.updateVideoLayersMutex.Lock()
	defer fake.updateVideoLayersMutex.Unlock()
	fake.UpdateVideoLayersStub = stub
}

func (fake *FakePublishedTrack) UpdateVideoLayersArgsForCall(i int) []*livekit.VideoLayer {
	fake.updateVideoLayersMutex.RLock()
	defer fake.updateVideoLayersMutex.RUnlock()
	argsForCall := fake.updateVideoLayersArgsForCall[i]
	return argsForCall.arg1
}

func (fake *FakePublishedTrack) Invocations() map[string][][]interface{} {
	fake.invocationsMutex.RLock()
	defer fake.invocationsMutex.RUnlock()
	fake.addOnCloseMutex.RLock()
	defer fake.addOnCloseMutex.RUnlock()
	fake.addSubscriberMutex.RLock()
	defer fake.addSubscriberMutex.RUnlock()
	fake.getQualityForDimensionMutex.RLock()
	defer fake.getQualityForDimensionMutex.RUnlock()
	fake.iDMutex.RLock()
	defer fake.iDMutex.RUnlock()
	fake.isMutedMutex.RLock()
	defer fake.isMutedMutex.RUnlock()
	fake.isSimulcastMutex.RLock()
	defer fake.isSimulcastMutex.RUnlock()
	fake.isSubscriberMutex.RLock()
	defer fake.isSubscriberMutex.RUnlock()
	fake.kindMutex.RLock()
	defer fake.kindMutex.RUnlock()
	fake.nameMutex.RLock()
	defer fake.nameMutex.RUnlock()
	fake.notifySubscriberMaxQualityMutex.RLock()
	defer fake.notifySubscriberMaxQualityMutex.RUnlock()
	fake.notifySubscriberMuteMutex.RLock()
	defer fake.notifySubscriberMuteMutex.RUnlock()
	fake.numUpTracksMutex.RLock()
	defer fake.numUpTracksMutex.RUnlock()
	fake.onSubscribedMaxQualityChangeMutex.RLock()
	defer fake.onSubscribedMaxQualityChangeMutex.RUnlock()
	fake.publishLossPercentageMutex.RLock()
	defer fake.publishLossPercentageMutex.RUnlock()
	fake.receiverMutex.RLock()
	defer fake.receiverMutex.RUnlock()
	fake.removeAllSubscribersMutex.RLock()
	defer fake.removeAllSubscribersMutex.RUnlock()
	fake.removeSubscriberMutex.RLock()
	defer fake.removeSubscriberMutex.RUnlock()
	fake.sdpCidMutex.RLock()
	defer fake.sdpCidMutex.RUnlock()
	fake.setMutedMutex.RLock()
	defer fake.setMutedMutex.RUnlock()
	fake.signalCidMutex.RLock()
	defer fake.signalCidMutex.RUnlock()
	fake.sourceMutex.RLock()
	defer fake.sourceMutex.RUnlock()
	fake.toProtoMutex.RLock()
	defer fake.toProtoMutex.RUnlock()
	fake.updateVideoLayersMutex.RLock()
	defer fake.updateVideoLayersMutex.RUnlock()
	copiedInvocations := map[string][][]interface{}{}
	for key, value := range fake.invocations {
		copiedInvocations[key] = value
	}
	return copiedInvocations
}

func (fake *FakePublishedTrack) recordInvocation(key string, args []interface{}) {
	fake.invocationsMutex.Lock()
	defer fake.invocationsMutex.Unlock()
	if fake.invocations == nil {
		fake.invocations = map[string][][]interface{}{}
	}
	if fake.invocations[key] == nil {
		fake.invocations[key] = [][]interface{}{}
	}
	fake.invocations[key] = append(fake.invocations[key], args)
}

var _ types.PublishedTrack = new(FakePublishedTrack)
