package test

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
	"testing"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/logger"
	livekit "github.com/livekit/protocol/proto"
	"github.com/livekit/protocol/utils"
	"github.com/livekit/protocol/webhook"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/service"
	"github.com/livekit/livekit-server/pkg/testutils"
)

func TestWebhooks(t *testing.T) {
	server, ts, finish, err := setupServerWithWebhook()
	require.NoError(t, err)
	defer finish()

	c1 := createRTCClient("c1", defaultServerPort, nil)
	waitUntilConnected(t, c1)
	testutils.WithTimeout(t, "webhook events room_started and participant_joined", func() bool {
		if ts.GetEvent(webhook.EventRoomStarted) == nil {
			return false
		}
		if ts.GetEvent(webhook.EventParticipantJoined) == nil {
			return false
		}
		return true
	})

	// first participant join should have started the room
	started := ts.GetEvent(webhook.EventRoomStarted)
	require.Equal(t, testRoom, started.Room.Name)
	joined := ts.GetEvent(webhook.EventParticipantJoined)
	require.Equal(t, "c1", joined.Participant.Identity)
	ts.ClearEvents()

	// another participant joins
	c2 := createRTCClient("c2", defaultServerPort, nil)
	waitUntilConnected(t, c2)
	defer c2.Stop()
	testutils.WithTimeout(t, "webhook events participant_joined", func() bool {
		if ts.GetEvent(webhook.EventParticipantJoined) == nil {
			return false
		}
		return true
	})
	joined = ts.GetEvent(webhook.EventParticipantJoined)
	require.Equal(t, "c2", joined.Participant.Identity)
	ts.ClearEvents()

	// first participant leaves
	c1.Stop()
	testutils.WithTimeout(t, "webhook events participant_left", func() bool {
		if ts.GetEvent(webhook.EventParticipantLeft) == nil {
			return false
		}
		return true
	})
	left := ts.GetEvent(webhook.EventParticipantLeft)
	require.Equal(t, "c1", left.Participant.Identity)
	ts.ClearEvents()

	// room closed
	rm := server.RoomManager().GetRoom(context.Background(), testRoom)
	rm.Close()
	testutils.WithTimeout(t, "webhook events room_finished", func() bool {
		if ts.GetEvent(webhook.EventRoomFinished) == nil {
			return false
		}
		return true
	})
	require.Equal(t, testRoom, ts.GetEvent(webhook.EventRoomFinished).Room.Name)
}

func setupServerWithWebhook() (server *service.LivekitServer, testServer *webookTestServer, finishFunc func(), err error) {
	conf, err := config.NewConfig("", nil)
	if err != nil {
		panic(fmt.Sprintf("could not create config: %v", err))
	}
	conf.WebHook.URLs = []string{"http://localhost:7890"}
	conf.WebHook.APIKey = testApiKey
	conf.Development = true
	conf.Keys = map[string]string{testApiKey: testApiSecret}

	testServer = newTestServer(":7890")
	if err = testServer.Start(); err != nil {
		return
	}

	currentNode, err := routing.NewLocalNode(conf)
	if err != nil {
		return
	}
	currentNode.Id = utils.NewGuid(nodeId1)

	server, err = service.InitializeServer(conf, currentNode)

	if err != nil {
		return
	}

	go func() {
		if err := server.Start(); err != nil {
			logger.Errorw("server returned error", err)
		}
	}()

	waitForServerToStart(server)

	finishFunc = func() {
		server.Stop(true)
		testServer.Stop()
	}
	return
}

type webookTestServer struct {
	server   *http.Server
	events   map[string]*livekit.WebhookEvent
	lock     sync.Mutex
	provider auth.KeyProvider
}

func newTestServer(addr string) *webookTestServer {
	s := &webookTestServer{
		events:   make(map[string]*livekit.WebhookEvent),
		provider: auth.NewFileBasedKeyProviderFromMap(map[string]string{testApiKey: testApiSecret}),
	}
	s.server = &http.Server{
		Addr:    addr,
		Handler: s,
	}
	return s
}

func (s *webookTestServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	data, err := webhook.Receive(r, s.provider)
	if err != nil {
		logger.Errorw("could not receive webhook", err)
		return
	}

	event := livekit.WebhookEvent{}
	if err = protojson.Unmarshal(data, &event); err != nil {
		logger.Errorw("could not unmarshal event", err)
		return
	}

	s.lock.Lock()
	s.events[event.Event] = &event
	s.lock.Unlock()
}

func (s *webookTestServer) GetEvent(name string) *livekit.WebhookEvent {
	s.lock.Lock()
	defer s.lock.Unlock()
	return s.events[name]
}

func (s *webookTestServer) ClearEvents() {
	s.lock.Lock()
	s.events = make(map[string]*livekit.WebhookEvent)
	s.lock.Unlock()
}

func (s *webookTestServer) Start() error {
	l, err := net.Listen("tcp", s.server.Addr)
	if err != nil {
		return err
	}
	go s.server.Serve(l)
	return nil
}

func (s *webookTestServer) Stop() {
	_ = s.server.Shutdown(context.Background())
}
