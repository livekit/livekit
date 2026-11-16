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


package service

import (
	"context"
	"net/netip"
	"testing"
)

func SetLookupNetIP(t *testing.T, fn func(ctx context.Context, network, host string) ([]netip.Addr, error)) {
	prev := lookupNetIP
	lookupNetIP = fn
	t.Cleanup(func() { lookupNetIP = prev })
}
