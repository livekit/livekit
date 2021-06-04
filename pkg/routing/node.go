package routing

import (
	"context"
	"crypto/sha1"
	"fmt"
	"os"
	"runtime"
	"time"

	"github.com/jxskiss/base62"
	"github.com/pion/stun"
	"github.com/pkg/errors"

	"github.com/livekit/livekit-server/pkg/config"
	livekit "github.com/livekit/livekit-server/proto"
	"github.com/livekit/protocol/utils"
)

type NodeStats struct {
	NumRooms         int32
	NumClients       int32
	NumVideoChannels int32
	NumAudioChannels int32
	BytesPerMin      int64
}

type LocalNode *livekit.Node

func NewLocalNode(conf *config.Config) (LocalNode, error) {
	ip, err := GetLocalIP(conf.RTC.StunServers)
	if err != nil {
		return nil, err
	}
	hostname, err := os.Hostname()
	if err != nil {
		return nil, err
	}
	return &livekit.Node{
		Id:      fmt.Sprintf("%s%s", utils.NodePrefix, HashedID(hostname)[:8]),
		Ip:      ip,
		NumCpus: uint32(runtime.NumCPU()),
		Stats: &livekit.NodeStats{
			StartedAt: time.Now().Unix(),
			UpdatedAt: time.Now().Unix(),
		},
	}, nil
}

func GetLocalIP(stunServers []string) (string, error) {
	if len(stunServers) == 0 {
		return "", errors.New("STUN servers are required but not defined")
	}
	c, err := stun.Dial("udp4", stunServers[0])
	if err != nil {
		return "", err
	}
	defer c.Close()

	message, err := stun.Build(stun.TransactionID, stun.BindingRequest)
	if err != nil {
		return "", err
	}

	var stunErr error
	// sufficiently large buffer to not block it
	ipChan := make(chan string, 20)
	err = c.Start(message, func(res stun.Event) {
		if res.Error != nil {
			stunErr = res.Error
			return
		}

		var xorAddr stun.XORMappedAddress
		if err := xorAddr.GetFrom(res.Message); err != nil {
			stunErr = err
			return
		}
		ip := xorAddr.IP.To4()
		if ip != nil {
			ipChan <- ip.String()
		}
	})
	if err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	select {
	case nodeIP := <-ipChan:
		return nodeIP, nil
	case <-ctx.Done():
		msg := "could not determine public IP"
		if stunErr != nil {
			return "", errors.Wrap(stunErr, msg)
		} else {
			return "", fmt.Errorf(msg)
		}
	}
}

// Creates a hashed ID from a unique string
func HashedID(id string) string {
	h := sha1.New()
	h.Write([]byte(id))
	val := h.Sum(nil)

	return base62.EncodeToString(val)
}
