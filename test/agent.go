package test

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sync"

	"github.com/gorilla/websocket"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/utils"
)

type agentClient struct {
	mu   sync.Mutex
	conn *websocket.Conn

	registered              bool
	roomAvailability        int
	roomJobs                int
	participantAvailability int
	participantJobs         int

	done chan struct{}
}

func newAgentClient(token string) (*agentClient, error) {
	host := fmt.Sprintf("ws://localhost:%d", defaultServerPort)
	u, err := url.Parse(host + "/agent")
	if err != nil {
		return nil, err
	}
	requestHeader := make(http.Header)
	requestHeader.Set("Authorization", "Bearer "+token)

	connectUrl := u.String()
	conn, _, err := websocket.DefaultDialer.Dial(connectUrl, requestHeader)
	if err != nil {
		return nil, err
	}

	return &agentClient{
		conn: conn,
		done: make(chan struct{}),
	}, nil
}

func (c *agentClient) Run() error {
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)

	go c.read()

	workerID := utils.NewGuid("W_")

	if err := c.write(&livekit.WorkerMessage{
		Message: &livekit.WorkerMessage_Register{
			Register: &livekit.RegisterWorkerRequest{
				Type:     livekit.JobType_JT_ROOM,
				WorkerId: workerID,
				Version:  "version",
				Name:     "name",
			},
		},
	}); err != nil {
		return err
	}

	if err := c.write(&livekit.WorkerMessage{
		Message: &livekit.WorkerMessage_Register{
			Register: &livekit.RegisterWorkerRequest{
				Type:     livekit.JobType_JT_PARTICIPANT,
				WorkerId: workerID,
				Version:  "version",
				Name:     "name",
			},
		},
	}); err != nil {
		return err
	}

	for {
		select {
		case <-interrupt:
			close(c.done)
		}
	}
}

func (c *agentClient) read() {
	for {
		_, b, err := c.conn.ReadMessage()
		if err != nil {
			return
		}

		msg := &livekit.ServerMessage{}
		if err = proto.Unmarshal(b, msg); err != nil {
			return
		}

		switch m := msg.Message.(type) {
		case *livekit.ServerMessage_Assignment:
			go c.handleAssignment(m.Assignment)
		case *livekit.ServerMessage_Availability:
			go c.handleAvailability(m.Availability)
		case *livekit.ServerMessage_Register:
			go c.handleRegister(m.Register)
		}
	}
}

func (c *agentClient) handleAssignment(req *livekit.JobAssignment) {
	if req.Job.Type == livekit.JobType_JT_ROOM {
		c.roomJobs++
	} else {
		c.participantJobs++
	}
}

func (c *agentClient) handleAvailability(req *livekit.AvailabilityRequest) {
	if req.Job.Type == livekit.JobType_JT_ROOM {
		c.roomAvailability++
	} else {
		c.participantAvailability++
	}

	c.write(&livekit.WorkerMessage{
		Message: &livekit.WorkerMessage_Availability{
			Availability: &livekit.AvailabilityResponse{
				JobId:     req.Job.Id,
				Available: true,
			},
		},
	})
}

func (c *agentClient) handleRegister(req *livekit.RegisterWorkerResponse) {
	c.registered = true
}

func (c *agentClient) write(msg *livekit.WorkerMessage) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	b, err := proto.Marshal(msg)
	if err != nil {
		return err
	}

	return c.conn.WriteMessage(websocket.BinaryMessage, b)
}

func (c *agentClient) Close() {
	close(c.done)
	_ = c.conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
	_ = c.conn.Close()
}
