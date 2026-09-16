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

package service_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/livekit-server/pkg/service"
)

// a listener stopped before it has served anything must not race its own accept
// loop: under -race this is what catches webtransport.Server.Serve running
// concurrently with Server.Close.
func TestWebTransportStopBeforeFirstConnection(t *testing.T) {
	for range 20 {
		wt := service.NewWebTransportServer(selfSignedTLS(t))
		wt.H3.Handler = service.NewWebTransportHandler(nil, wt, http.NewServeMux())
		_, err := wt.Listen([]string{"127.0.0.1"}, 0)
		require.NoError(t, err)
		require.NoError(t, wt.Shutdown(context.Background()))
	}
}
