package datachannel

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/pion/datachannel"

	"github.com/livekit/protocol/utils/mono"
)

const (
	singleWriteTimeout = 50 * time.Millisecond
)

var (
	ErrDataDroppedBySlowReader         = errors.New("data dropped by slow reader")
	ErrDataDroppedByHighBufferedAmount = errors.New("data dropped due to high buffered amount")
)

type BufferedAmountGetter interface {
	BufferedAmount() uint64
}

type DataChannelWriter[T BufferedAmountGetter] struct {
	bufferGetter      T
	rawDC             datachannel.ReadWriteCloserDeadliner
	slowThreshold     int
	rate              *BitrateCalculator
	reliable          bool
	targetLatency     time.Duration
	minBufferedAmount uint64
}

// NewDataChannelWriterReliable creates a new DataChannelWriter for reliable data channel by
// detaching it, when writing to the datachanel times out, it will block and retry if the
// receiver's bitrate is above the slowThreshold or drop the data if it's below the threshold.
// If the slowThreshold is 0, it will never retry on write timeout.
func NewDataChannelWriterReliable[T BufferedAmountGetter](bufferGetter T, rawDC datachannel.ReadWriteCloserDeadliner, slowThreshold int) *DataChannelWriter[T] {
	var rate *BitrateCalculator
	if slowThreshold > 0 {
		rate = NewBitrateCalculator(BitrateDuration, BitrateWindow)
	}
	return &DataChannelWriter[T]{
		bufferGetter:  bufferGetter,
		rawDC:         rawDC,
		slowThreshold: slowThreshold,
		rate:          rate,
		reliable:      true,
	}
}

// NewDataChannelWriterUnreliable creates a new DataChannelWriter for unreliable data channel.
// It will drop data when the buffered amount is too high to maintain the target latency.
// The latency is estimated based on the bitrate in past 1 second. If targetLatency is 0, no
// buffering control is applied.
func NewDataChannelWriterUnreliable[T BufferedAmountGetter](bufferGetter T, rawDC datachannel.ReadWriteCloserDeadliner, targetLatency time.Duration, minBufferedAmount uint64) *DataChannelWriter[T] {
	var rate *BitrateCalculator
	if targetLatency > 0 {
		rate = NewBitrateCalculator(BitrateDuration, BitrateWindow)
	}
	return &DataChannelWriter[T]{
		bufferGetter:      bufferGetter,
		rawDC:             rawDC,
		rate:              rate,
		targetLatency:     targetLatency,
		minBufferedAmount: minBufferedAmount,
		reliable:          false,
	}
}

func (w *DataChannelWriter[T]) BufferedAmountGetter() T {
	return w.bufferGetter
}

func (w *DataChannelWriter[T]) Write(p []byte) (n int, err error) {
	if w.reliable {
		return w.writeReliable(p)
	} else {
		return w.writeUnreliable(p)
	}
}

func (w *DataChannelWriter[T]) writeReliable(p []byte) (n int, err error) {
	for {
		err = w.rawDC.SetWriteDeadline(time.Now().Add(singleWriteTimeout))
		if err != nil {
			return 0, err
		}
		n, err = w.rawDC.Write(p)
		if w.slowThreshold == 0 {
			return
		}

		now := mono.Now()
		w.rate.AddBytes(n, int(w.bufferGetter.BufferedAmount()), now)
		// retry if the write timed out on a non-slow receiver
		if errors.Is(err, context.DeadlineExceeded) {
			if bitrate, ok := w.rate.Bitrate(now); !ok || bitrate >= w.slowThreshold {
				continue
			} else {
				err = fmt.Errorf("%w: bitrate %d, threshold %d", ErrDataDroppedBySlowReader, bitrate, w.slowThreshold)
			}
		}

		return
	}
}

func (w *DataChannelWriter[T]) writeUnreliable(p []byte) (n int, err error) {
	if w.targetLatency == 0 {
		err = w.rawDC.SetWriteDeadline(time.Now().Add(singleWriteTimeout))
		if err != nil {
			return 0, err
		}
		return w.rawDC.Write(p)
	}

	if bitrate, ok := w.rate.Bitrate(time.Now()); ok {
		// control buffer latency to ~100ms
		if w.bufferGetter.BufferedAmount() > uint64(time.Duration(bitrate)*w.targetLatency/8/time.Second) && w.bufferGetter.BufferedAmount() > w.minBufferedAmount {
			return 0, ErrDataDroppedByHighBufferedAmount
		}
	}

	err = w.rawDC.SetWriteDeadline(time.Now().Add(singleWriteTimeout))
	if err != nil {
		return 0, err
	}
	n, err = w.rawDC.Write(p)
	if err != nil {
		w.rate.AddBytes(n, int(w.bufferGetter.BufferedAmount()), mono.Now())
	}

	return
}

func (w *DataChannelWriter[T]) Close() error {
	return w.rawDC.Close()
}
