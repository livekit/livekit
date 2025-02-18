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

var ErrDataDroppedBySlowReader = errors.New("data dropped by slow reader")

type BufferedAmountGetter interface {
	BufferedAmount() uint64
}

type DataChannelWriter[T BufferedAmountGetter] struct {
	bufferGetter  T
	rawDC         datachannel.ReadWriteCloserDeadliner
	slowThreshold int
	rate          *BitrateCalculator
}

// NewDataChannelWriter creates a new DataChannelWriter by detaching the data channel, when
// writing to the datachanel times out, it will block and retry if the receiver's bitrate is
// above the slowThreshold or drop the data if it's below the threshold. If the slowThreshold
// is 0, it will never retry on write timeout.
func NewDataChannelWriter[T BufferedAmountGetter](bufferGetter T, rawDC datachannel.ReadWriteCloserDeadliner, slowThreshold int) *DataChannelWriter[T] {
	var rate *BitrateCalculator
	if slowThreshold > 0 {
		rate = NewBitrateCalculator(BitrateDuration, BitrateWindow)
	}
	return &DataChannelWriter[T]{
		bufferGetter:  bufferGetter,
		rawDC:         rawDC,
		slowThreshold: slowThreshold,
		rate:          rate,
	}
}

func (w *DataChannelWriter[T]) BufferedAmountGetter() T {
	return w.bufferGetter
}

func (w *DataChannelWriter[T]) Write(p []byte) (n int, err error) {
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

func (w *DataChannelWriter[T]) Close() error {
	return w.rawDC.Close()
}
