// Copyright 2026 LiveKit, Inc.
// SPDX-License-Identifier: Apache-2.0

package moq

import (
	"context"
	"sync"
	"time"

	"github.com/trickstercache/trickster/v2/pkg/moq/message"
	"github.com/trickstercache/trickster/v2/pkg/moq/session"
)

// Use disjoint alias ranges for bidirectional clients that keep a single alias table.
// Client publications in the LiveKit profile use the lower half.
func moqSubscriberAlias(r *session.Request) uint64 {
	return (1 << 31) + r.Session().NewTrackAlias()
}

// The goroutine writing objects also answers updates so the final stream count
// includes every subgroup it opened before the update arrived.
type moqSubscription struct {
	update chan struct{}
	once   sync.Once
}

func acceptMoQSubscription(r *session.Request) (*moqSubscription, error) {
	if err := r.Send(&message.SubscribeOK{TrackAlias: moqSubscriberAlias(r)}); err != nil {
		r.Cancel(message.ResetInternalError)
		return nil, err
	}
	s := &moqSubscription{update: make(chan struct{})}
	r.SetHandlers(s, nil)
	return s, nil
}

func (s *moqSubscription) HandleMessage(_ *session.Request, msg message.Message) {
	if _, ok := msg.(*message.RequestUpdate); ok {
		s.once.Do(func() { close(s.update) })
	}
}

func (*moqSubscription) HandleClose(*session.Request, error) {}

func endMoQSubscriptionOnUpdate(r *session.Request) {
	const reason = "subscription updates are unsupported on this live track"
	if err := r.Send(&message.RequestError{Code: message.RequestNotSupported, Reason: reason}); err != nil {
		r.Cancel(message.ResetInternalError)
		return
	}
	if err := r.Send(&message.PublishDone{Code: message.DoneUpdateFailed, StreamCount: r.StreamCount(), Reason: reason}); err != nil {
		r.Cancel(message.ResetInternalError)
		return
	}
	_ = r.CloseSend()
}

func writeMoQObject(r *session.Request, group uint64, data []byte) error {
	return writeMoQSample(r, message.SubgroupHeader{Group: group, Priority: 128, EndOfGroup: true}, message.SubgroupObject{PayloadLen: uint64(len(data))}, data)
}

func writeMoQSample(r *session.Request, header message.SubgroupHeader, obj message.SubgroupObject, data []byte) error {
	// One bounded writer per subscription can wait for QUIC path validation
	// and retransmission while the client moves to a different network.
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	stream, err := r.OpenSubgroup(ctx, header, 3, false)
	if err != nil {
		return err
	}
	deadline, _ := ctx.Deadline()
	if err = stream.SetWriteDeadline(deadline); err == nil {
		err = stream.WriteObject(&obj)
	}
	if err == nil {
		_, err = stream.Write(data)
	}
	if err != nil {
		stream.Reset(message.ResetCancelled)
		return err
	}
	return stream.Close()
}
