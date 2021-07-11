package routing

import (
	"crypto/sha1"
	"fmt"
	"os"
	"runtime"
	"time"

	"github.com/jxskiss/base62"

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
	hostname, err := os.Hostname()
	if err != nil {
		return nil, err
	}
	if conf.RTC.NodeIP == "" {
		return nil, ErrIPNotSet
	}
	return &livekit.Node{
		Id:      fmt.Sprintf("%s%s", utils.NodePrefix, HashedID(hostname)[:8]),
		Ip:      conf.RTC.NodeIP,
		NumCpus: uint32(runtime.NumCPU()),
		Stats: &livekit.NodeStats{
			StartedAt: time.Now().Unix(),
			UpdatedAt: time.Now().Unix(),
		},
	}, nil
}

// Creates a hashed ID from a unique string
func HashedID(id string) string {
	h := sha1.New()
	h.Write([]byte(id))
	val := h.Sum(nil)

	return base62.EncodeToString(val)
}
