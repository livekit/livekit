package rpc_test

import (
	context "context"
	"errors"
	"testing"
	"time"

	"github.com/livekit/livekit-server/pkg/service/rpc"
	"github.com/stretchr/testify/require"
)

type raceTestValue struct {
	v int
}

func TestRaceOne(t *testing.T) {
	r := rpc.NewRace[raceTestValue](context.Background())
	r.Go(func(ctx context.Context) (*raceTestValue, error) {
		return &raceTestValue{0}, nil
	})
	i, res, err := r.Wait()
	require.EqualValues(t, 0, i)
	require.EqualValues(t, &raceTestValue{0}, res)
	require.NoError(t, err)
}

func TestRaceError(t *testing.T) {
	r := rpc.NewRace[raceTestValue](context.Background())
	r.Go(func(ctx context.Context) (*raceTestValue, error) {
		return nil, errors.New("oh no...")
	})
	i, res, err := r.Wait()
	require.EqualValues(t, 0, i)
	require.Nil(t, res)
	require.Error(t, err)
}

func TestRaceMany(t *testing.T) {
	r := rpc.NewRace[raceTestValue](context.Background())
	for i := 0; i < 5; i++ {
		i := i
		r.Go(func(ctx context.Context) (*raceTestValue, error) {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(i) * time.Millisecond):
				return &raceTestValue{i}, nil
			}
		})
	}
	i, res, err := r.Wait()
	require.EqualValues(t, 0, i)
	require.EqualValues(t, &raceTestValue{0}, res)
	require.NoError(t, err)
}
