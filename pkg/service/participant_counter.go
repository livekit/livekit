package service

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	p2p_database "github.com/dTelecom/p2p-realtime-database"
	"github.com/ipfs/go-log/v2"
	"github.com/pkg/errors"
)

const prefixParticipantCounterKey = "participant_counter_"

const (
	intervalPingParticipantCounts = 5 * time.Second
	deadlinePingParticipantCounts = 30 * time.Second
)

type ParticipantCounter struct {
	mainDatabase      *p2p_database.DB
	regularExpression *regexp.Regexp
	lock              sync.Mutex
	logger            *log.ZapEventLogger
}

func NewParticipantCounter(mainDatabase *p2p_database.DB, logger *log.ZapEventLogger) (*ParticipantCounter, error) {
	regularExpression, err := regexp.Compile(`participant_counter_([a-zA-Z0-9]+)_([a-zA-Z0-9]+)_([a-zA-Z0-9]+)`)
	if err != nil {
		return nil, errors.Wrap(err, "regexp compile")
	}

	participantCounter := &ParticipantCounter{
		mainDatabase:      mainDatabase,
		lock:              sync.Mutex{},
		regularExpression: regularExpression,
		logger:            logger,
	}

	err = participantCounter.refreshParticipantsTTL(context.Background())
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

		parts := c.regularExpression.FindStringSubmatch(k)
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

func (c *ParticipantCounter) generateKey(clientApiKey, participantId string) string {
	return c.generatePrefixNode() + clientApiKey + "_" + participantId
}

func (c *ParticipantCounter) generatePrefixNode() string {
	return prefixParticipantCounterKey + c.mainDatabase.GetHost().ID().String() + "_"
}
