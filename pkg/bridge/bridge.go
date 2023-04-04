package bridge

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/nats-io/nats.go"
)

type Bridge struct {
	conn *nats.Conn
	js   nats.JetStreamContext
}

func NewBridge(conf *config.Config) (*Bridge, error) {
	url := conf.Bridge.Url
	token := conf.Bridge.Token

	nc, err := nats.Connect(
		url,
		nats.MaxReconnects(20),
		nats.ReconnectWait(60*time.Second),
		nats.Token(token),
		nats.DisconnectErrHandler(func(nc *nats.Conn, err error) {
			// handle disconnect
			fmt.Println("NATS Status:", nc.Status().String())
			if !nc.IsReconnecting() {
				panic("Disconnected from NATS!")
			}
		}),
		nats.ReconnectHandler(func(nc *nats.Conn) {
			// handle reconnect
			fmt.Println("NATS Status:", nc.Status().String())
		}),
	)
	if err != nil {
		return nil, err
	}

	// Jetstream
	js, err := nc.JetStream()
	if err != nil {
		return nil, err
	}

	// Add wave stream
	_, err = js.AddStream(&nats.StreamConfig{
		Name: "wave",
		Subjects: []string{
			"wave.events",
		},
		Retention: nats.InterestPolicy,
		Replicas:  1,
	})
	if err != nil {
		return nil, err
	}

	bc := &Bridge{
		conn: nc,
		js:   js,
	}
	return bc, nil
}

func (b *Bridge) StreamPublish(subject string, payload interface{}) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = b.js.Publish(subject, data)
	if err != nil {
		return err
	}
	return nil
}
