package routing

import (
	"context"
	"errors"
	"sync"
	"time"

	"go.uber.org/atomic"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/telemetry/prometheus"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/protocol/utils"
	"github.com/livekit/psrpc"
	"github.com/livekit/psrpc/pkg/middleware"
)

var ErrSignalWriteFailed = errors.New("signal write failed")
var ErrSignalMessageDropped = errors.New("signal message dropped")

//go:generate go run github.com/maxbrunsfeld/counterfeiter/v6 -generate

//counterfeiter:generate . SignalClient
type SignalClient interface {
	ActiveCount() int
	StartParticipantSignal(ctx context.Context, roomName livekit.RoomName, pi ParticipantInit, nodeID livekit.NodeID) (connectionID livekit.ConnectionID, reqSink MessageSink, resSource MessageSource, err error)
}

type signalClient struct {
	nodeID livekit.NodeID
	config config.SignalRelayConfig
	client rpc.TypedSignalClient
	active atomic.Int32
}

func NewSignalClient(nodeID livekit.NodeID, bus psrpc.MessageBus, config config.SignalRelayConfig) (SignalClient, error) {
	c, err := rpc.NewTypedSignalClient(
		nodeID,
		bus,
		middleware.WithClientMetrics(prometheus.PSRPCMetricsObserver{}),
		psrpc.WithClientChannelSize(config.StreamBufferSize),
	)
	if err != nil {
		return nil, err
	}

	return &signalClient{
		nodeID: nodeID,
		config: config,
		client: c,
	}, nil
}

func (r *signalClient) ActiveCount() int {
	return int(r.active.Load())
}

func (r *signalClient) StartParticipantSignal(
	ctx context.Context,
	roomName livekit.RoomName,
	pi ParticipantInit,
	nodeID livekit.NodeID,
) (
	connectionID livekit.ConnectionID,
	reqSink MessageSink,
	resSource MessageSource,
	err error,
) {
	connectionID = livekit.ConnectionID(utils.NewGuid("CO_"))
	ss, err := pi.ToStartSession(roomName, connectionID)
	if err != nil {
		return
	}

	l := logger.GetLogger().WithValues(
		"room", roomName,
		"reqNodeID", nodeID,
		"participant", pi.Identity,
		"connectionID", connectionID,
	)

	l.Debugw("starting signal connection")

	stream, err := r.client.RelaySignal(ctx, nodeID)
	if err != nil {
		prometheus.MessageCounter.WithLabelValues("signal", "failure").Add(1)
		return
	}

	err = stream.Send(&rpc.RelaySignalRequest{StartSession: ss})
	if err != nil {
		stream.Close(err)
		prometheus.MessageCounter.WithLabelValues("signal", "failure").Add(1)
		return
	}

	sink := NewSignalMessageSink(SignalSinkParams[*rpc.RelaySignalRequest, *rpc.RelaySignalResponse]{
		Logger:         l,
		Stream:         stream,
		Config:         r.config,
		Writer:         signalRequestMessageWriter{},
		CloseOnFailure: true,
	})
	resChan := NewDefaultMessageChannel()

	go func() {
		r.active.Inc()
		defer r.active.Dec()

		err := CopySignalStreamToMessageChannel[*rpc.RelaySignalRequest, *rpc.RelaySignalResponse](
			stream,
			resChan,
			signalResponseMessageReader{},
			r.config,
		)
		l.Infow("signal stream closed", "error", err)

		resChan.Close()
	}()

	return connectionID, sink, resChan, nil
}

type signalRequestMessageWriter struct{}

func (e signalRequestMessageWriter) Write(seq uint64, close bool, msgs []proto.Message) *rpc.RelaySignalRequest {
	r := &rpc.RelaySignalRequest{
		Seq:      seq,
		Requests: make([]*livekit.SignalRequest, 0, len(msgs)),
		Close:    close,
	}
	for _, m := range msgs {
		r.Requests = append(r.Requests, m.(*livekit.SignalRequest))
	}
	return r
}

type signalResponseMessageReader struct{}

func (e signalResponseMessageReader) Read(rm *rpc.RelaySignalResponse) ([]proto.Message, error) {
	msgs := make([]proto.Message, 0, len(rm.Responses))
	for _, m := range rm.Responses {
		msgs = append(msgs, m)
	}
	return msgs, nil
}

type RelaySignalMessage interface {
	proto.Message
	GetSeq() uint64
	GetClose() bool
}

type SignalMessageWriter[SendType RelaySignalMessage] interface {
	Write(seq uint64, close bool, msgs []proto.Message) SendType
}

type SignalMessageReader[RecvType RelaySignalMessage] interface {
	Read(msg RecvType) ([]proto.Message, error)
}

func CopySignalStreamToMessageChannel[SendType, RecvType RelaySignalMessage](
	stream psrpc.Stream[SendType, RecvType],
	ch *MessageChannel,
	reader SignalMessageReader[RecvType],
	config config.SignalRelayConfig,
) error {
	r := &signalMessageReader[SendType, RecvType]{
		reader: reader,
		config: config,
	}
	for msg := range stream.Channel() {
		res, err := r.Read(msg)
		if err != nil {
			prometheus.MessageCounter.WithLabelValues("signal", "failure").Add(1)
			return err
		}

		for _, r := range res {
			if err = ch.WriteMessage(r); err != nil {
				prometheus.MessageCounter.WithLabelValues("signal", "failure").Add(1)
				return err
			}
			prometheus.MessageCounter.WithLabelValues("signal", "success").Add(1)
		}

		if msg.GetClose() {
			return stream.Close(nil)
		}
	}
	return stream.Err()
}

type signalMessageReader[SendType, RecvType RelaySignalMessage] struct {
	seq    uint64
	reader SignalMessageReader[RecvType]
	config config.SignalRelayConfig
}

func (r *signalMessageReader[SendType, RecvType]) Read(msg RecvType) ([]proto.Message, error) {
	res, err := r.reader.Read(msg)
	if err != nil {
		return nil, err
	}

	if r.seq < msg.GetSeq() {
		return nil, ErrSignalMessageDropped
	}
	if r.seq > msg.GetSeq() {
		n := int(r.seq - msg.GetSeq())
		if n > len(res) {
			n = len(res)
		}
		res = res[n:]
	}
	r.seq += uint64(len(res))

	return res, nil
}

type SignalSinkParams[SendType, RecvType RelaySignalMessage] struct {
	Stream         psrpc.Stream[SendType, RecvType]
	Logger         logger.Logger
	Config         config.SignalRelayConfig
	Writer         SignalMessageWriter[SendType]
	CloseOnFailure bool
}

func NewSignalMessageSink[SendType, RecvType RelaySignalMessage](params SignalSinkParams[SendType, RecvType]) MessageSink {
	return &signalMessageSink[SendType, RecvType]{
		SignalSinkParams: params,
	}
}

type signalMessageSink[SendType, RecvType RelaySignalMessage] struct {
	SignalSinkParams[SendType, RecvType]

	mu       sync.Mutex
	seq      uint64
	queue    []proto.Message
	writing  bool
	draining bool
}

func (s *signalMessageSink[SendType, RecvType]) Close() {
	s.mu.Lock()
	s.draining = true
	if !s.writing {
		s.writing = true
		go s.write()
	}
	s.mu.Unlock()

	// NOTE: not waiting for stream context to be done.
	// Waiting for stream context to be done is confirmation
	// that the close message has been processed by the other side.
	// In ideal conditions, waiting for it is a clean end.
	//
	// But, in cases where the remote side goes away abruptly, waiting
	// for stream context to be done could block connection progress
	// till the timeout hits.
	//
	// The abrupt case happens when one side of the signal
	// relay is shut down due to scale down or a crash.
	//
	// Uncomment the following line to wait for close acknowledgement,
	// but the system should be able to wait long enough (till timeout)
	// without adverse impact if waiting for close acknowledgement.
	//<-s.Stream.Context().Done()
}

func (s *signalMessageSink[SendType, RecvType]) IsClosed() bool {
	return s.Stream.Err() != nil
}

func (s *signalMessageSink[SendType, RecvType]) write() {
	interval := s.Config.MinRetryInterval
	deadline := time.Now().Add(s.Config.RetryTimeout)
	var err error

	s.mu.Lock()
	for {
		close := s.draining
		if (!close && len(s.queue) == 0) || s.IsClosed() {
			break
		}
		msg, n := s.Writer.Write(s.seq, close, s.queue), len(s.queue)
		s.mu.Unlock()

		err = s.Stream.Send(msg, psrpc.WithTimeout(interval))
		if err != nil {
			if time.Now().After(deadline) {
				s.Logger.Warnw("could not send signal message", err)

				s.mu.Lock()
				s.seq += uint64(len(s.queue))
				s.queue = nil
				break
			}

			interval *= 2
			if interval > s.Config.MaxRetryInterval {
				interval = s.Config.MaxRetryInterval
			}
		}

		s.mu.Lock()
		if err == nil {
			interval = s.Config.MinRetryInterval
			deadline = time.Now().Add(s.Config.RetryTimeout)

			s.seq += uint64(n)
			s.queue = s.queue[n:]

			if close {
				break
			}
		}
	}

	s.writing = false
	if s.draining {
		s.Stream.Close(nil)
	}
	if err != nil && s.CloseOnFailure {
		s.Stream.Close(ErrSignalWriteFailed)
	}
	s.mu.Unlock()
}

func (s *signalMessageSink[SendType, RecvType]) WriteMessage(msg proto.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.Stream.Err(); err != nil {
		return err
	} else if s.draining {
		return psrpc.ErrStreamClosed
	}

	s.queue = append(s.queue, msg)
	if !s.writing {
		s.writing = true
		go s.write()
	}
	return nil
}
