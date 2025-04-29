package datachannel

import (
	"context"
	"testing"
	"time"

	"github.com/pion/datachannel"
	"github.com/pion/transport/v3/deadline"
	"github.com/stretchr/testify/require"
)

func TestDataChannelWriter(t *testing.T) {
	mockDC := newMockDataChannelWriter()
	// slow threshold is 1000B/s
	w := NewDataChannelWriter(mockDC, mockDC, 8000)
	require.Equal(t, mockDC, w.BufferedAmountGetter())
	buf := make([]byte, 2000)
	// write 2000 bytes so it should not drop in 2 seconds
	t0 := time.Now()
	n, err := w.Write(buf)
	require.NoError(t, err)
	require.Equal(t, 2000, n)

	t1 := time.Now()
	mockDC.SetNextWriteCompleteAt(t0.Add(time.Second * 3 / 2))
	n, err = w.Write(buf[:10])
	require.NoError(t, err)
	require.Equal(t, 10, n)
	require.GreaterOrEqual(t, time.Since(t1), time.Second)

	// bitrate below slow threshold(2000bytes/3sec), should drop by timeout
	mockDC.SetNextWriteCompleteAt(t0.Add(3 * time.Second))
	n, err = w.Write(buf[:1000])
	require.ErrorIs(t, err, ErrDataDroppedBySlowReader, err)
	require.Equal(t, 0, n)
}

func TestDataChannelWriter_NoSlowThreshold(t *testing.T) {
	mockDC := newMockDataChannelWriter()
	w := NewDataChannelWriter(mockDC, mockDC, 0)
	buf := make([]byte, 2000)
	n, err := w.Write(buf)
	require.NoError(t, err)
	require.Equal(t, 2000, n)
	mockDC.SetNextWriteCompleteAt(time.Now().Add(singleWriteTimeout / 2))
	n, err = w.Write(buf[:10])
	require.NoError(t, err)
	require.Equal(t, 10, n)

	// slow threshold is 0, should not block & retry
	mockDC.SetNextWriteCompleteAt(time.Now().Add(singleWriteTimeout * 2))
	n, err = w.Write(buf[:1000])
	require.ErrorIs(t, err, context.DeadlineExceeded, err)
	require.Equal(t, 0, n)
}

type mockDataChannelWriter struct {
	datachannel.ReadWriteCloserDeadliner
	nextWriteCompleteAt time.Time
	deadline            *deadline.Deadline
}

func newMockDataChannelWriter() *mockDataChannelWriter {
	return &mockDataChannelWriter{
		deadline: deadline.New(),
	}
}

func (m *mockDataChannelWriter) BufferedAmount() uint64 {
	return 0
}

func (m *mockDataChannelWriter) Write(b []byte) (int, error) {
	wait := time.Until(m.nextWriteCompleteAt)
	if wait <= 0 {
		return len(b), nil
	}
	select {
	case <-m.deadline.Done():
		return 0, m.deadline.Err()
	case <-time.After(wait):
		return len(b), nil
	}
}

func (m *mockDataChannelWriter) SetWriteDeadline(t time.Time) error {
	m.deadline.Set(t)
	return nil
}

func (m *mockDataChannelWriter) SetNextWriteCompleteAt(t time.Time) {
	m.nextWriteCompleteAt = t
}
