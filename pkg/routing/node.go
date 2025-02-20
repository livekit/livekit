package routing

import (
	"fmt"
	"runtime"
	"time"

	"github.com/gagliardetto/solana-go"
	"github.com/livekit/protocol/livekit"

	"github.com/livekit/livekit-server/pkg/config"
)

type LocalNode *livekit.Node

func NewLocalNode(conf *config.Config) (LocalNode, error) {
	if conf.RTC.NodeIP == "" {
		return nil, ErrIPNotSet
	}

	privateKey, err := solana.PrivateKeyFromBase58(conf.Solana.WalletPrivateKey)
	if err != nil {
		return nil, fmt.Errorf("invalid private key: %v", err)
	}

	node := &livekit.Node{
		Id:      privateKey.PublicKey().String(),
		Ip:      conf.RTC.NodeIP,
		NumCpus: uint32(runtime.NumCPU()),
		Region:  conf.Region,
		State:   livekit.NodeState_SERVING,
		Stats: &livekit.NodeStats{
			StartedAt: time.Now().Unix(),
			UpdatedAt: time.Now().Unix(),
		},
	}

	return node, nil
}
