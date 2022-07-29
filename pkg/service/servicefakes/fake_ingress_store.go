// Code generated by counterfeiter. DO NOT EDIT.
package servicefakes

import (
	"context"
	"sync"

	"github.com/livekit/livekit-server/pkg/service"
	"github.com/livekit/protocol/livekit"
)

type FakeIngressStore struct {
	DeleteIngressStub        func(context.Context, *livekit.IngressInfo) error
	deleteIngressMutex       sync.RWMutex
	deleteIngressArgsForCall []struct {
		arg1 context.Context
		arg2 *livekit.IngressInfo
	}
	deleteIngressReturns struct {
		result1 error
	}
	deleteIngressReturnsOnCall map[int]struct {
		result1 error
	}
	ListIngressStub        func(context.Context, livekit.RoomName) ([]*livekit.IngressInfo, error)
	listIngressMutex       sync.RWMutex
	listIngressArgsForCall []struct {
		arg1 context.Context
		arg2 livekit.RoomName
	}
	listIngressReturns struct {
		result1 []*livekit.IngressInfo
		result2 error
	}
	listIngressReturnsOnCall map[int]struct {
		result1 []*livekit.IngressInfo
		result2 error
	}
	LoadIngressStub        func(context.Context, string) (*livekit.IngressInfo, error)
	loadIngressMutex       sync.RWMutex
	loadIngressArgsForCall []struct {
		arg1 context.Context
		arg2 string
	}
	loadIngressReturns struct {
		result1 *livekit.IngressInfo
		result2 error
	}
	loadIngressReturnsOnCall map[int]struct {
		result1 *livekit.IngressInfo
		result2 error
	}
	LoadIngressFromStreamKeyStub        func(context.Context, string) (*livekit.IngressInfo, error)
	loadIngressFromStreamKeyMutex       sync.RWMutex
	loadIngressFromStreamKeyArgsForCall []struct {
		arg1 context.Context
		arg2 string
	}
	loadIngressFromStreamKeyReturns struct {
		result1 *livekit.IngressInfo
		result2 error
	}
	loadIngressFromStreamKeyReturnsOnCall map[int]struct {
		result1 *livekit.IngressInfo
		result2 error
	}
	StoreIngressStub        func(context.Context, *livekit.IngressInfo) error
	storeIngressMutex       sync.RWMutex
	storeIngressArgsForCall []struct {
		arg1 context.Context
		arg2 *livekit.IngressInfo
	}
	storeIngressReturns struct {
		result1 error
	}
	storeIngressReturnsOnCall map[int]struct {
		result1 error
	}
	UpdateIngressStub        func(context.Context, *livekit.IngressInfo) error
	updateIngressMutex       sync.RWMutex
	updateIngressArgsForCall []struct {
		arg1 context.Context
		arg2 *livekit.IngressInfo
	}
	updateIngressReturns struct {
		result1 error
	}
	updateIngressReturnsOnCall map[int]struct {
		result1 error
	}
	invocations      map[string][][]interface{}
	invocationsMutex sync.RWMutex
}

func (fake *FakeIngressStore) DeleteIngress(arg1 context.Context, arg2 *livekit.IngressInfo) error {
	fake.deleteIngressMutex.Lock()
	ret, specificReturn := fake.deleteIngressReturnsOnCall[len(fake.deleteIngressArgsForCall)]
	fake.deleteIngressArgsForCall = append(fake.deleteIngressArgsForCall, struct {
		arg1 context.Context
		arg2 *livekit.IngressInfo
	}{arg1, arg2})
	stub := fake.DeleteIngressStub
	fakeReturns := fake.deleteIngressReturns
	fake.recordInvocation("DeleteIngress", []interface{}{arg1, arg2})
	fake.deleteIngressMutex.Unlock()
	if stub != nil {
		return stub(arg1, arg2)
	}
	if specificReturn {
		return ret.result1
	}
	return fakeReturns.result1
}

func (fake *FakeIngressStore) DeleteIngressCallCount() int {
	fake.deleteIngressMutex.RLock()
	defer fake.deleteIngressMutex.RUnlock()
	return len(fake.deleteIngressArgsForCall)
}

func (fake *FakeIngressStore) DeleteIngressCalls(stub func(context.Context, *livekit.IngressInfo) error) {
	fake.deleteIngressMutex.Lock()
	defer fake.deleteIngressMutex.Unlock()
	fake.DeleteIngressStub = stub
}

func (fake *FakeIngressStore) DeleteIngressArgsForCall(i int) (context.Context, *livekit.IngressInfo) {
	fake.deleteIngressMutex.RLock()
	defer fake.deleteIngressMutex.RUnlock()
	argsForCall := fake.deleteIngressArgsForCall[i]
	return argsForCall.arg1, argsForCall.arg2
}

func (fake *FakeIngressStore) DeleteIngressReturns(result1 error) {
	fake.deleteIngressMutex.Lock()
	defer fake.deleteIngressMutex.Unlock()
	fake.DeleteIngressStub = nil
	fake.deleteIngressReturns = struct {
		result1 error
	}{result1}
}

func (fake *FakeIngressStore) DeleteIngressReturnsOnCall(i int, result1 error) {
	fake.deleteIngressMutex.Lock()
	defer fake.deleteIngressMutex.Unlock()
	fake.DeleteIngressStub = nil
	if fake.deleteIngressReturnsOnCall == nil {
		fake.deleteIngressReturnsOnCall = make(map[int]struct {
			result1 error
		})
	}
	fake.deleteIngressReturnsOnCall[i] = struct {
		result1 error
	}{result1}
}

func (fake *FakeIngressStore) ListIngress(arg1 context.Context, arg2 livekit.RoomName) ([]*livekit.IngressInfo, error) {
	fake.listIngressMutex.Lock()
	ret, specificReturn := fake.listIngressReturnsOnCall[len(fake.listIngressArgsForCall)]
	fake.listIngressArgsForCall = append(fake.listIngressArgsForCall, struct {
		arg1 context.Context
		arg2 livekit.RoomName
	}{arg1, arg2})
	stub := fake.ListIngressStub
	fakeReturns := fake.listIngressReturns
	fake.recordInvocation("ListIngress", []interface{}{arg1, arg2})
	fake.listIngressMutex.Unlock()
	if stub != nil {
		return stub(arg1, arg2)
	}
	if specificReturn {
		return ret.result1, ret.result2
	}
	return fakeReturns.result1, fakeReturns.result2
}

func (fake *FakeIngressStore) ListIngressCallCount() int {
	fake.listIngressMutex.RLock()
	defer fake.listIngressMutex.RUnlock()
	return len(fake.listIngressArgsForCall)
}

func (fake *FakeIngressStore) ListIngressCalls(stub func(context.Context, livekit.RoomName) ([]*livekit.IngressInfo, error)) {
	fake.listIngressMutex.Lock()
	defer fake.listIngressMutex.Unlock()
	fake.ListIngressStub = stub
}

func (fake *FakeIngressStore) ListIngressArgsForCall(i int) (context.Context, livekit.RoomName) {
	fake.listIngressMutex.RLock()
	defer fake.listIngressMutex.RUnlock()
	argsForCall := fake.listIngressArgsForCall[i]
	return argsForCall.arg1, argsForCall.arg2
}

func (fake *FakeIngressStore) ListIngressReturns(result1 []*livekit.IngressInfo, result2 error) {
	fake.listIngressMutex.Lock()
	defer fake.listIngressMutex.Unlock()
	fake.ListIngressStub = nil
	fake.listIngressReturns = struct {
		result1 []*livekit.IngressInfo
		result2 error
	}{result1, result2}
}

func (fake *FakeIngressStore) ListIngressReturnsOnCall(i int, result1 []*livekit.IngressInfo, result2 error) {
	fake.listIngressMutex.Lock()
	defer fake.listIngressMutex.Unlock()
	fake.ListIngressStub = nil
	if fake.listIngressReturnsOnCall == nil {
		fake.listIngressReturnsOnCall = make(map[int]struct {
			result1 []*livekit.IngressInfo
			result2 error
		})
	}
	fake.listIngressReturnsOnCall[i] = struct {
		result1 []*livekit.IngressInfo
		result2 error
	}{result1, result2}
}

func (fake *FakeIngressStore) LoadIngress(arg1 context.Context, arg2 string) (*livekit.IngressInfo, error) {
	fake.loadIngressMutex.Lock()
	ret, specificReturn := fake.loadIngressReturnsOnCall[len(fake.loadIngressArgsForCall)]
	fake.loadIngressArgsForCall = append(fake.loadIngressArgsForCall, struct {
		arg1 context.Context
		arg2 string
	}{arg1, arg2})
	stub := fake.LoadIngressStub
	fakeReturns := fake.loadIngressReturns
	fake.recordInvocation("LoadIngress", []interface{}{arg1, arg2})
	fake.loadIngressMutex.Unlock()
	if stub != nil {
		return stub(arg1, arg2)
	}
	if specificReturn {
		return ret.result1, ret.result2
	}
	return fakeReturns.result1, fakeReturns.result2
}

func (fake *FakeIngressStore) LoadIngressCallCount() int {
	fake.loadIngressMutex.RLock()
	defer fake.loadIngressMutex.RUnlock()
	return len(fake.loadIngressArgsForCall)
}

func (fake *FakeIngressStore) LoadIngressCalls(stub func(context.Context, string) (*livekit.IngressInfo, error)) {
	fake.loadIngressMutex.Lock()
	defer fake.loadIngressMutex.Unlock()
	fake.LoadIngressStub = stub
}

func (fake *FakeIngressStore) LoadIngressArgsForCall(i int) (context.Context, string) {
	fake.loadIngressMutex.RLock()
	defer fake.loadIngressMutex.RUnlock()
	argsForCall := fake.loadIngressArgsForCall[i]
	return argsForCall.arg1, argsForCall.arg2
}

func (fake *FakeIngressStore) LoadIngressReturns(result1 *livekit.IngressInfo, result2 error) {
	fake.loadIngressMutex.Lock()
	defer fake.loadIngressMutex.Unlock()
	fake.LoadIngressStub = nil
	fake.loadIngressReturns = struct {
		result1 *livekit.IngressInfo
		result2 error
	}{result1, result2}
}

func (fake *FakeIngressStore) LoadIngressReturnsOnCall(i int, result1 *livekit.IngressInfo, result2 error) {
	fake.loadIngressMutex.Lock()
	defer fake.loadIngressMutex.Unlock()
	fake.LoadIngressStub = nil
	if fake.loadIngressReturnsOnCall == nil {
		fake.loadIngressReturnsOnCall = make(map[int]struct {
			result1 *livekit.IngressInfo
			result2 error
		})
	}
	fake.loadIngressReturnsOnCall[i] = struct {
		result1 *livekit.IngressInfo
		result2 error
	}{result1, result2}
}

func (fake *FakeIngressStore) LoadIngressFromStreamKey(arg1 context.Context, arg2 string) (*livekit.IngressInfo, error) {
	fake.loadIngressFromStreamKeyMutex.Lock()
	ret, specificReturn := fake.loadIngressFromStreamKeyReturnsOnCall[len(fake.loadIngressFromStreamKeyArgsForCall)]
	fake.loadIngressFromStreamKeyArgsForCall = append(fake.loadIngressFromStreamKeyArgsForCall, struct {
		arg1 context.Context
		arg2 string
	}{arg1, arg2})
	stub := fake.LoadIngressFromStreamKeyStub
	fakeReturns := fake.loadIngressFromStreamKeyReturns
	fake.recordInvocation("LoadIngressFromStreamKey", []interface{}{arg1, arg2})
	fake.loadIngressFromStreamKeyMutex.Unlock()
	if stub != nil {
		return stub(arg1, arg2)
	}
	if specificReturn {
		return ret.result1, ret.result2
	}
	return fakeReturns.result1, fakeReturns.result2
}

func (fake *FakeIngressStore) LoadIngressFromStreamKeyCallCount() int {
	fake.loadIngressFromStreamKeyMutex.RLock()
	defer fake.loadIngressFromStreamKeyMutex.RUnlock()
	return len(fake.loadIngressFromStreamKeyArgsForCall)
}

func (fake *FakeIngressStore) LoadIngressFromStreamKeyCalls(stub func(context.Context, string) (*livekit.IngressInfo, error)) {
	fake.loadIngressFromStreamKeyMutex.Lock()
	defer fake.loadIngressFromStreamKeyMutex.Unlock()
	fake.LoadIngressFromStreamKeyStub = stub
}

func (fake *FakeIngressStore) LoadIngressFromStreamKeyArgsForCall(i int) (context.Context, string) {
	fake.loadIngressFromStreamKeyMutex.RLock()
	defer fake.loadIngressFromStreamKeyMutex.RUnlock()
	argsForCall := fake.loadIngressFromStreamKeyArgsForCall[i]
	return argsForCall.arg1, argsForCall.arg2
}

func (fake *FakeIngressStore) LoadIngressFromStreamKeyReturns(result1 *livekit.IngressInfo, result2 error) {
	fake.loadIngressFromStreamKeyMutex.Lock()
	defer fake.loadIngressFromStreamKeyMutex.Unlock()
	fake.LoadIngressFromStreamKeyStub = nil
	fake.loadIngressFromStreamKeyReturns = struct {
		result1 *livekit.IngressInfo
		result2 error
	}{result1, result2}
}

func (fake *FakeIngressStore) LoadIngressFromStreamKeyReturnsOnCall(i int, result1 *livekit.IngressInfo, result2 error) {
	fake.loadIngressFromStreamKeyMutex.Lock()
	defer fake.loadIngressFromStreamKeyMutex.Unlock()
	fake.LoadIngressFromStreamKeyStub = nil
	if fake.loadIngressFromStreamKeyReturnsOnCall == nil {
		fake.loadIngressFromStreamKeyReturnsOnCall = make(map[int]struct {
			result1 *livekit.IngressInfo
			result2 error
		})
	}
	fake.loadIngressFromStreamKeyReturnsOnCall[i] = struct {
		result1 *livekit.IngressInfo
		result2 error
	}{result1, result2}
}

func (fake *FakeIngressStore) StoreIngress(arg1 context.Context, arg2 *livekit.IngressInfo) error {
	fake.storeIngressMutex.Lock()
	ret, specificReturn := fake.storeIngressReturnsOnCall[len(fake.storeIngressArgsForCall)]
	fake.storeIngressArgsForCall = append(fake.storeIngressArgsForCall, struct {
		arg1 context.Context
		arg2 *livekit.IngressInfo
	}{arg1, arg2})
	stub := fake.StoreIngressStub
	fakeReturns := fake.storeIngressReturns
	fake.recordInvocation("StoreIngress", []interface{}{arg1, arg2})
	fake.storeIngressMutex.Unlock()
	if stub != nil {
		return stub(arg1, arg2)
	}
	if specificReturn {
		return ret.result1
	}
	return fakeReturns.result1
}

func (fake *FakeIngressStore) StoreIngressCallCount() int {
	fake.storeIngressMutex.RLock()
	defer fake.storeIngressMutex.RUnlock()
	return len(fake.storeIngressArgsForCall)
}

func (fake *FakeIngressStore) StoreIngressCalls(stub func(context.Context, *livekit.IngressInfo) error) {
	fake.storeIngressMutex.Lock()
	defer fake.storeIngressMutex.Unlock()
	fake.StoreIngressStub = stub
}

func (fake *FakeIngressStore) StoreIngressArgsForCall(i int) (context.Context, *livekit.IngressInfo) {
	fake.storeIngressMutex.RLock()
	defer fake.storeIngressMutex.RUnlock()
	argsForCall := fake.storeIngressArgsForCall[i]
	return argsForCall.arg1, argsForCall.arg2
}

func (fake *FakeIngressStore) StoreIngressReturns(result1 error) {
	fake.storeIngressMutex.Lock()
	defer fake.storeIngressMutex.Unlock()
	fake.StoreIngressStub = nil
	fake.storeIngressReturns = struct {
		result1 error
	}{result1}
}

func (fake *FakeIngressStore) StoreIngressReturnsOnCall(i int, result1 error) {
	fake.storeIngressMutex.Lock()
	defer fake.storeIngressMutex.Unlock()
	fake.StoreIngressStub = nil
	if fake.storeIngressReturnsOnCall == nil {
		fake.storeIngressReturnsOnCall = make(map[int]struct {
			result1 error
		})
	}
	fake.storeIngressReturnsOnCall[i] = struct {
		result1 error
	}{result1}
}

func (fake *FakeIngressStore) UpdateIngress(arg1 context.Context, arg2 *livekit.IngressInfo) error {
	fake.updateIngressMutex.Lock()
	ret, specificReturn := fake.updateIngressReturnsOnCall[len(fake.updateIngressArgsForCall)]
	fake.updateIngressArgsForCall = append(fake.updateIngressArgsForCall, struct {
		arg1 context.Context
		arg2 *livekit.IngressInfo
	}{arg1, arg2})
	stub := fake.UpdateIngressStub
	fakeReturns := fake.updateIngressReturns
	fake.recordInvocation("UpdateIngress", []interface{}{arg1, arg2})
	fake.updateIngressMutex.Unlock()
	if stub != nil {
		return stub(arg1, arg2)
	}
	if specificReturn {
		return ret.result1
	}
	return fakeReturns.result1
}

func (fake *FakeIngressStore) UpdateIngressCallCount() int {
	fake.updateIngressMutex.RLock()
	defer fake.updateIngressMutex.RUnlock()
	return len(fake.updateIngressArgsForCall)
}

func (fake *FakeIngressStore) UpdateIngressCalls(stub func(context.Context, *livekit.IngressInfo) error) {
	fake.updateIngressMutex.Lock()
	defer fake.updateIngressMutex.Unlock()
	fake.UpdateIngressStub = stub
}

func (fake *FakeIngressStore) UpdateIngressArgsForCall(i int) (context.Context, *livekit.IngressInfo) {
	fake.updateIngressMutex.RLock()
	defer fake.updateIngressMutex.RUnlock()
	argsForCall := fake.updateIngressArgsForCall[i]
	return argsForCall.arg1, argsForCall.arg2
}

func (fake *FakeIngressStore) UpdateIngressReturns(result1 error) {
	fake.updateIngressMutex.Lock()
	defer fake.updateIngressMutex.Unlock()
	fake.UpdateIngressStub = nil
	fake.updateIngressReturns = struct {
		result1 error
	}{result1}
}

func (fake *FakeIngressStore) UpdateIngressReturnsOnCall(i int, result1 error) {
	fake.updateIngressMutex.Lock()
	defer fake.updateIngressMutex.Unlock()
	fake.UpdateIngressStub = nil
	if fake.updateIngressReturnsOnCall == nil {
		fake.updateIngressReturnsOnCall = make(map[int]struct {
			result1 error
		})
	}
	fake.updateIngressReturnsOnCall[i] = struct {
		result1 error
	}{result1}
}

func (fake *FakeIngressStore) Invocations() map[string][][]interface{} {
	fake.invocationsMutex.RLock()
	defer fake.invocationsMutex.RUnlock()
	fake.deleteIngressMutex.RLock()
	defer fake.deleteIngressMutex.RUnlock()
	fake.listIngressMutex.RLock()
	defer fake.listIngressMutex.RUnlock()
	fake.loadIngressMutex.RLock()
	defer fake.loadIngressMutex.RUnlock()
	fake.loadIngressFromStreamKeyMutex.RLock()
	defer fake.loadIngressFromStreamKeyMutex.RUnlock()
	fake.storeIngressMutex.RLock()
	defer fake.storeIngressMutex.RUnlock()
	fake.updateIngressMutex.RLock()
	defer fake.updateIngressMutex.RUnlock()
	copiedInvocations := map[string][][]interface{}{}
	for key, value := range fake.invocations {
		copiedInvocations[key] = value
	}
	return copiedInvocations
}

func (fake *FakeIngressStore) recordInvocation(key string, args []interface{}) {
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

var _ service.IngressStore = new(FakeIngressStore)
