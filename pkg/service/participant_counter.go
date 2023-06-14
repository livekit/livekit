package service

import (
	"context"
	"errors"
	"fmt"
	p2p_database "github.com/dTelecom/p2p-realtime-database"
	"strconv"
	"sync"
)

const prefixParticipantCounterKey = "participant_counter_"

type ParticipantCounter struct {
	mainDatabase *p2p_database.DB
	lock         sync.Mutex
}

func NewParticipantCounter(mainDatabase *p2p_database.DB) *ParticipantCounter {
	return &ParticipantCounter{
		mainDatabase: mainDatabase,
		lock:         sync.Mutex{},
	}
}

func (c *ParticipantCounter) Increment(ctx context.Context, peerId string) error {
	c.lock.Lock()
	defer c.lock.Unlock()

	current, err := c.GetCurrentValue(ctx, peerId)
	if err != nil {
		return err
	}
	current++

	return c.mainDatabase.Set(ctx, c.generateKey(peerId), strconv.Itoa(current))
}

func (c *ParticipantCounter) Decrement(ctx context.Context, peerId string) error {
	c.lock.Lock()
	defer c.lock.Unlock()

	current, err := c.GetCurrentValue(ctx, peerId)
	if err != nil {
		return err
	}

	if current == 0 {
		return nil
	}
	current--

	return c.mainDatabase.Set(ctx, c.generateKey(peerId), strconv.Itoa(current))
}

func (c *ParticipantCounter) GetCurrentValue(ctx context.Context, peerId string) (int, error) {
	var currentCounter int

	currentCounterValue, err := c.mainDatabase.Get(ctx, c.generateKey(peerId))
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

func (c *ParticipantCounter) generateKey(peerId string) string {
	return prefixParticipantCounterKey + peerId
}
