package service

import (
	"context"
	"errors"
	"fmt"
	p2p_database "github.com/dTelecom/p2p-realtime-database"
	"github.com/livekit/protocol/livekit"
	"strconv"
	"sync"
)

const prefixParticipantCounterKey = "participant_counter_"

type ParticipantCounter struct {
	currentNodeId livekit.NodeID
	mainDatabase  *p2p_database.DB
	lock          sync.Mutex
}

func NewParticipantCounter(currentNodeId livekit.NodeID, mainDatabase *p2p_database.DB) *ParticipantCounter {
	counter := &ParticipantCounter{
		currentNodeId: currentNodeId,
		mainDatabase:  mainDatabase,
		lock:          sync.Mutex{},
	}

	return counter
}

func (c *ParticipantCounter) Increment(ctx context.Context) error {
	c.lock.Lock()
	defer c.lock.Unlock()

	current, err := c.getCurrentValue(ctx)
	if err != nil {
		return err
	}
	current++

	return c.mainDatabase.Set(ctx, c.generateKey(), strconv.Itoa(current))
}

func (c *ParticipantCounter) Decrement(ctx context.Context) error {
	c.lock.Lock()
	defer c.lock.Unlock()

	current, err := c.getCurrentValue(ctx)
	if err != nil {
		return err
	}

	if current == 0 {
		return nil
	}
	current--

	return c.mainDatabase.Set(ctx, c.generateKey(), strconv.Itoa(current))
}

func (c *ParticipantCounter) getCurrentValue(ctx context.Context) (int, error) {
	var currentCounter int

	currentCounterValue, err := c.mainDatabase.Get(ctx, c.generateKey())
	switch {
	case errors.Is(err, p2p_database.ErrKeyNotFound):
		currentCounter = 0
	case err != nil:
		return 0, fmt.Errorf("get current value: %w", err)
	default:
		currentCounter, err = strconv.Atoi(currentCounterValue)
		if err != nil {
			return 0, fmt.Errorf("convert current value to int: %w", err)
		}
	}

	return currentCounter, nil
}

func (c *ParticipantCounter) generateKey() string {
	return prefixParticipantCounterKey + string(c.currentNodeId)
}
