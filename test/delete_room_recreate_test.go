package test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/livekit"
)

func TestSingleNodeDeleteRoomRecreateGetsNewSID(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
		return
	}

	_, finish := setupSingleNodeTest("TestSingleNodeDeleteRoomRecreateGetsNewSID")
	defer finish()

	createCtx := contextWithToken(createRoomToken())

	for _, testRTCServicePath := range testRTCServicePaths {
		t.Run(fmt.Sprintf("testRTCServicePath=%s", testRTCServicePath.String()), func(t *testing.T) {
			firstRoom, err := roomClient.CreateRoom(createCtx, &livekit.CreateRoomRequest{
				Name:             testRoom,
				DepartureTimeout: 1,
				EmptyTimeout:     30,
			})
			require.NoError(t, err)

			c1 := createRTCClient("delete-recreate-1", defaultServerPort, testRTCServicePath, nil)
			waitUntilConnected(t, c1)

			_, err = roomClient.DeleteRoom(createCtx, &livekit.DeleteRoomRequest{
				Room: testRoom,
			})
			require.NoError(t, err)

			secondRoom, err := roomClient.CreateRoom(createCtx, &livekit.CreateRoomRequest{
				Name:             testRoom,
				DepartureTimeout: 1,
				EmptyTimeout:     30,
			})
			require.NoError(t, err)
			require.NotEqual(t, firstRoom.Sid, secondRoom.Sid)
			require.GreaterOrEqual(t, secondRoom.CreationTimeMs, firstRoom.CreationTimeMs)

			c2 := createRTCClient("delete-recreate-2", defaultServerPort, testRTCServicePath, nil)
			waitUntilConnected(t, c2)
			stopClients(c2)

			_, err = roomClient.DeleteRoom(createCtx, &livekit.DeleteRoomRequest{
				Room: testRoom,
			})
			require.NoError(t, err)
		})
	}
}
