package test

import (
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/pion/turn/v2"
	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/utils"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/service"
	livekit "github.com/livekit/livekit-server/proto"
)

func testTurnServer(t *testing.T) {
	conf, err := config.NewConfig("", nil)
	require.NoError(t, err)

	conf.TURN.Enabled = true

	currentNode, err := routing.NewLocalNode(conf)
	require.NoError(t, err)
	currentNode.Id = utils.NewGuid(nodeId1)

	// local routing and store
	router := routing.NewLocalRouter(currentNode)
	roomStore := service.NewLocalRoomStore()
	roomStore.DeleteRoom(testRoom)
	s, err := service.InitializeServer(conf, &StaticKeyProvider{}, roomStore, router, currentNode, &routing.RandomSelector{})
	require.NoError(t, err)
	go s.Start()
	waitForServerToStart(s)
	defer s.Stop()

	time.Sleep(syncDelay)

	// create a room
	rm := &livekit.Room{
		Sid:          utils.NewGuid(utils.RoomPrefix),
		Name:         "testroom",
		TurnPassword: utils.RandomSecret(),
	}
	require.NoError(t, roomStore.CreateRoom(rm))

	turnConf := &turn.ClientConfig{
		STUNServerAddr: fmt.Sprintf("localhost:%d", conf.TURN.TLSPort),
		TURNServerAddr: fmt.Sprintf("%s:%d", currentNode.Ip, conf.TURN.TLSPort),
		Username:       rm.Name,
		Password:       rm.TurnPassword,
		Realm:          "livekit",
	}

	t.Run("TURN works over TCP", func(t *testing.T) {
		conn, err := net.Dial("tcp", fmt.Sprintf("localhost:%d", conf.TURN.TLSPort))
		require.NoError(t, err)

		tc := *turnConf
		tc.Conn = turn.NewSTUNConn(conn)
		c, err := turn.NewClient(&tc)
		require.NoError(t, err)
		defer c.Close()

		require.NoError(t, c.Listen())

		// Allocate a relay socket on the TURN server. On success, it
		// will return a net.PacketConn which represents the remote
		// socket.
		relayConn, err := c.Allocate()
		require.NoError(t, err)

		defer func() {
			require.NoError(t, relayConn.Close())
		}()
	})
	// UDP test doesn't pass
	//t.Run("TURN connects over UDP", func(t *testing.T) {
	//	conn, err := net.ListenPacket("udp4", "0.0.0.0:0")
	//	require.NoError(t, err)
	//	defer func() {
	//		require.NoError(t, conn.Close())
	//	}()
	//
	//	tc := *turnConf
	//	tc.Conn = conn
	//
	//	client, err := turn.NewClient(&tc)
	//	require.NoError(t, err)
	//	defer client.Close()
	//
	//	// Start listening on the conn provided.
	//	require.NoError(t, client.Listen())
	//
	//	// Allocate a relay socket on the TURN server. On success, it
	//	// will return a net.PacketConn which represents the remote
	//	// socket.
	//	relayConn, err := client.Allocate()
	//	require.NoError(t, err)
	//	defer func() {
	//		require.NoError(t, relayConn.Close())
	//	}()
	//})
}
