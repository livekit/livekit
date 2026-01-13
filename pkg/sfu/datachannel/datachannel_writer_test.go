package datachannel

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/datachannel"
	"github.com/pion/transport/v4/deadline"
	"github.com/stretchr/testify/require"
)

func TestDataChannelWriter(t *testing.T) {
	mockDC := newMockDataChannelWriter()
	// slow threshold is 1000B/s
	w := NewDataChannelWriterReliable(mockDC, mockDC, 8000)
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
	w := NewDataChannelWriterReliable(mockDC, mockDC, 0)
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

func TestDataChannelWriter_Unreliable(t *testing.T) {
	mockDC := newMockLossyDataChannelWriter(8192)
	w := NewDataChannelWriterUnreliable(mockDC, mockDC, 100*time.Millisecond, 2000)
	for range 10 {
		buf := make([]byte, 128)
		_, err := w.Write(buf)
		require.NoError(t, err)
		time.Sleep(100 * time.Millisecond)
	}
	buf := make([]byte, 4096)
	_, err := w.Write(buf)
	require.NoError(t, err)
	// should drop due to high buffered amount
	_, err = w.Write(buf)
	require.ErrorIs(t, err, ErrDataDroppedByHighBufferedAmount)
}

// mockDataChannelWriter
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

// mockLossyDataChannelWriter
type mockLossyDataChannelWriter struct {
	datachannel.ReadWriteCloserDeadliner
	bufferedAmount atomic.Int64
	targetBitrate  int
	lastWriteAt    time.Time
}

func newMockLossyDataChannelWriter(targetBitrate int) *mockLossyDataChannelWriter {
	return &mockLossyDataChannelWriter{
		targetBitrate: targetBitrate,
		lastWriteAt:   time.Now(),
	}
}

func (m *mockLossyDataChannelWriter) BufferedAmount() uint64 {
	return uint64(m.bufferedAmount.Load())
}

func (m *mockLossyDataChannelWriter) Write(b []byte) (int, error) {
	m.bufferedAmount.Add(int64(len(b)))
	if time.Now().Before(m.lastWriteAt) {
		return len(b), nil
	}

	// drain buffer based on target bitrate
	canWriteBytes := time.Since(m.lastWriteAt) * time.Duration(m.targetBitrate) / time.Second / 8
	if m.bufferedAmount.Load() <= int64(canWriteBytes) {
		m.lastWriteAt = m.lastWriteAt.Add(time.Duration(int64(time.Second) * int64(m.BufferedAmount()) / (int64(m.targetBitrate) / 8)))
		m.bufferedAmount.Store(0)
	} else {
		m.lastWriteAt = time.Now()
		m.bufferedAmount.Add(-int64(canWriteBytes))
	}
	return len(b), nil
}

func (m *mockLossyDataChannelWriter) SetWriteDeadline(t time.Time) error {
	return nil
}
