package service

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	p2p_database "github.com/dTelecom/p2p-realtime-database"
	"github.com/ipfs/go-log/v2"
	"github.com/pkg/errors"
)

const prefixParticipantCounterKey = "participant-counter_"

const (
	intervalPingParticipantCounts = 5 * time.Second
	deadlinePingParticipantCounts = 30 * time.Second
)

type ParticipantCounter struct {
	mainDatabase *p2p_database.DB
	lock         sync.Mutex
	logger       *log.ZapEventLogger
}

func NewParticipantCounter(mainDatabase *p2p_database.DB, logger *log.ZapEventLogger) (*ParticipantCounter, error) {
	participantCounter := &ParticipantCounter{
		mainDatabase: mainDatabase,
		lock:         sync.Mutex{},
		logger:       logger,
	}

	err := participantCounter.refreshParticipantsTTL(context.Background())
	if err != nil {
		return nil, errors.Wrap(err, "start process refresh participants")
	}

	return participantCounter, nil
}

func (c *ParticipantCounter) Increment(ctx context.Context, clientApiKey, participantId string) error {
	k := c.generateKey(clientApiKey, participantId)

	err := c.mainDatabase.Set(ctx, k, time.Now().String())
	if err != nil {
		return errors.Wrap(err, "set db")
	}

	err = c.mainDatabase.TTL(ctx, k, deadlinePingParticipantCounts)
	if err != nil {
		return errors.Wrap(err, "ttl db")
	}

	return nil
}

func (c *ParticipantCounter) Decrement(ctx context.Context, clientApiKey, participantId string) error {
	k := c.generateKey(clientApiKey, participantId)

	err := c.mainDatabase.Remove(ctx, k)
	if err != nil && !errors.Is(err, p2p_database.ErrKeyNotFound) {
		return errors.Wrap(err, "set db")
	}

	return nil
}

func (c *ParticipantCounter) GetCurrentValue(ctx context.Context, clientApiKey string) (int, error) {
	var currentCounter int

	keys, err := c.mainDatabase.List(ctx)
	if err != nil {
		return 0, fmt.Errorf("list p2p keys: %w", err)
	}

	for _, k := range keys {
		k = strings.TrimLeft(k, "/")
		if !strings.HasPrefix(k, prefixParticipantCounterKey) {
			continue
		}

		parts := strings.SplitN(k, "_", 4)
		if len(parts) == 0 {
			continue
		}
		if clientApiKey != parts[2] {
			continue
		}

		currentCounter++
	}

	return currentCounter, nil
}

func (c *ParticipantCounter) RemoveAllNodeKeys(ctx context.Context) error {
	keys, err := c.mainDatabase.List(ctx)
	if err != nil {
		return errors.Wrap(err, "p2p list")
	}
	for _, k := range keys {
		k = strings.TrimLeft("/", k)
		if !strings.HasPrefix(k, c.generatePrefixNode()) {
			continue
		}
		err = c.mainDatabase.Remove(ctx, k)
		if err != nil {
			c.logger.Errorw("remove key "+k, err)
		}
	}
	return nil
}

func (c *ParticipantCounter) refreshParticipantsTTL(ctx context.Context) error {
	prefixNode := c.generatePrefixNode()

	go func() {
		ticker := time.NewTicker(intervalPingParticipantCounts)
		for {
			keys, err := c.mainDatabase.List(ctx)
			if err != nil {
				c.logger.Errorw("list keys", err)
				continue
			}

			for _, k := range keys {
				k = strings.TrimLeft(k, "/")
				if !strings.HasPrefix(k, prefixNode) {
					continue
				}

				err = c.mainDatabase.TTL(ctx, k, deadlinePingParticipantCounts)
				if err != nil {
					c.logger.Errorw("update ttl", err)
				}
			}
			<-ticker.C
		}
	}()

	return nil
}

// participant-counter_{nodeId}_{clientKey}_{participantId}
func (c *ParticipantCounter) generateKey(clientApiKey, participantId string) string {
	return c.generatePrefixNode() + clientApiKey + "_" + participantId
}

func (c *ParticipantCounter) generatePrefixNode() string {
	return prefixParticipantCounterKey + c.mainDatabase.GetHost().ID().String() + "_"
}
