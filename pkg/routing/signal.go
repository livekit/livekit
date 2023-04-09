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
	"github.com/livekit/psrpc/middleware"
)

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
		return
	}

	err = stream.Send(&rpc.RelaySignalRequest{StartSession: ss})
	if err != nil {
		stream.Close(err)
		return
	}

	sink := NewSignalMessageSink[*rpc.RelaySignalRequest, *rpc.RelaySignalResponse](l, stream, r.config, signalRequestMessageWriter{}, true)
	resChan := NewDefaultMessageChannel()

	go func() {
		r.active.Inc()
		defer r.active.Dec()

		err = CopySignalStreamToMessageChannel[*rpc.RelaySignalRequest, *rpc.RelaySignalResponse](
			stream,
			resChan,
			signalResponseMessageReader{},
			r.config,
		)
		l.Debugw("participant signal stream closed", "error", err)

		resChan.Close()
	}()

	return connectionID, sink, resChan, nil
}

type signalRequestMessageWriter struct{}

func (e signalRequestMessageWriter) WriteOne(seq uint64, msg proto.Message) *rpc.RelaySignalRequest {
	return &rpc.RelaySignalRequest{
		Seq:     seq,
		Request: msg.(*livekit.SignalRequest),
	}
}

func (e signalRequestMessageWriter) WriteMany(seq uint64, msgs []proto.Message) *rpc.RelaySignalRequest {
	r := &rpc.RelaySignalRequest{
		Seq:      seq,
		Requests: make([]*livekit.SignalRequest, 0, len(msgs)),
	}
	for _, m := range msgs {
		r.Requests = append(r.Requests, m.(*livekit.SignalRequest))
	}
	return r
}

type signalResponseMessageReader struct{}

func (e signalResponseMessageReader) Read(rm *rpc.RelaySignalResponse) ([]proto.Message, error) {
	msgs := make([]proto.Message, 0, len(rm.Responses)+1)
	if rm.Response != nil {
		msgs = append(msgs, rm.Response)
	}
	for _, m := range rm.Responses {
		msgs = append(msgs, m)
	}
	return msgs, nil
}

type RelaySignalMessage interface {
	proto.Message
	GetSeq() uint64
}

type SignalMessageWriter[SendType RelaySignalMessage] interface {
	WriteOne(seq uint64, msg proto.Message) SendType
	WriteMany(seq uint64, msgs []proto.Message) SendType
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
		var res []proto.Message
		res, err := r.Read(msg)
		if err != nil {
			return err
		}
		for _, r := range res {
			if err = ch.WriteMessage(r); err != nil {
				return err
			}
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

	if r.config.MinVersion >= 1 {
		if r.seq < msg.GetSeq() {
			return nil, errors.New("participant signal message dropped")
		}
		if r.seq < msg.GetSeq() {
			res = res[msg.GetSeq()-r.seq:]
		}
		r.seq += uint64(len(res))
	}
	return res, nil
}

func NewSignalMessageSink[SendType, RecvType RelaySignalMessage](
	logger logger.Logger,
	stream psrpc.Stream[SendType, RecvType],
	config config.SignalRelayConfig,
	writer SignalMessageWriter[SendType],
	closeOnError bool,
) MessageSink {
	return &signalMessageSink[SendType, RecvType]{
		stream:       stream,
		logger:       logger,
		config:       config,
		writer:       writer,
		closeOnError: closeOnError,
	}
}

type signalMessageSink[SendType, RecvType RelaySignalMessage] struct {
	stream       psrpc.Stream[SendType, RecvType]
	logger       logger.Logger
	config       config.SignalRelayConfig
	writer       SignalMessageWriter[SendType]
	closeOnError bool

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
		s.stream.Close(nil)
	}
	s.mu.Unlock()
}

func (s *signalMessageSink[SendType, RecvType]) IsClosed() bool {
	return s.stream.Context().Err() != nil
}

func (s *signalMessageSink[SendType, RecvType]) write() {
	attempt := 0
	interval := s.config.MinRetryInterval
	deadline := time.Now().Add(s.config.RetryTimeout)

	s.mu.Lock()
	for {
		if len(s.queue) == 0 || s.IsClosed() {
			if s.draining {
				s.stream.Close(nil)
			}
			s.writing = false
			break
		}

		var msg SendType
		var n int
		if s.config.EnableBatching {
			msg = s.writer.WriteMany(s.seq, s.queue)
			n = len(s.queue)
		} else {
			msg = s.writer.WriteOne(s.seq, s.queue[0])
			n = 1
		}
		s.mu.Unlock()

		err := s.stream.Send(msg, psrpc.WithTimeout(interval))
		if err != nil {
			done := time.Now().After(deadline)

			s.logger.Warnw(
				"could not send message to participant", err,
				"attempt", attempt,
				"retry", !done,
			)

			if done {
				if s.closeOnError {
					s.stream.Close(nil)
				}
				return
			}

			attempt++
			interval *= 2
			if interval > s.config.MaxRetryInterval {
				interval = s.config.MaxRetryInterval
			}
		}

		s.mu.Lock()
		if err == nil {
			attempt = 0
			interval = s.config.MinRetryInterval
			deadline = time.Now().Add(s.config.RetryTimeout)

			s.queue = s.queue[n:]
			s.seq += uint64(n)
		}
	}
	s.mu.Unlock()
}

func (s *signalMessageSink[SendType, RecvType]) WriteMessage(msg proto.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.draining || s.IsClosed() {
		return psrpc.ErrStreamClosed
	}

	s.queue = append(s.queue, msg)
	if !s.writing {
		s.writing = true
		go s.write()
	}
	return nil
}
