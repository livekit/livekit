package sfu

import (
	"context"

	"github.com/pion/webrtc/v3"
)

type (
	// Datachannel is a wrapper to define middlewares executed on defined label.
	// The datachannels created will be negotiated on join to all peers that joins
	// the SFU.
	Datachannel struct {
		Label       string
		middlewares []func(MessageProcessor) MessageProcessor
		onMessage   func(ctx context.Context, args ProcessArgs)
	}

	ProcessArgs struct {
		Peer        Peer
		Message     webrtc.DataChannelMessage
		DataChannel *webrtc.DataChannel
	}

	Middlewares []func(MessageProcessor) MessageProcessor

	MessageProcessor interface {
		Process(ctx context.Context, args ProcessArgs)
	}

	ProcessFunc func(ctx context.Context, args ProcessArgs)

	chainHandler struct {
		middlewares Middlewares
		Last        MessageProcessor
		current     MessageProcessor
	}
)

// Use adds the middlewares to the current Datachannel.
// The middlewares are going to be executed before the OnMessage event fires.
func (dc *Datachannel) Use(middlewares ...func(MessageProcessor) MessageProcessor) {
	dc.middlewares = append(dc.middlewares, middlewares...)
}

// OnMessage sets the message callback for the datachannel, the event is fired
// after all the middlewares have processed the message.
func (dc *Datachannel) OnMessage(fn func(ctx context.Context, args ProcessArgs)) {
	dc.onMessage = fn
}

func (p ProcessFunc) Process(ctx context.Context, args ProcessArgs) {
	p(ctx, args)
}

func (mws Middlewares) Process(h MessageProcessor) MessageProcessor {
	return &chainHandler{mws, h, chain(mws, h)}
}

func (mws Middlewares) ProcessFunc(h MessageProcessor) MessageProcessor {
	return &chainHandler{mws, h, chain(mws, h)}
}

func newDCChain(m []func(p MessageProcessor) MessageProcessor) Middlewares {
	return Middlewares(m)
}

func (c *chainHandler) Process(ctx context.Context, args ProcessArgs) {
	c.current.Process(ctx, args)
}

func chain(mws []func(processor MessageProcessor) MessageProcessor, last MessageProcessor) MessageProcessor {
	if len(mws) == 0 {
		return last
	}
	h := mws[len(mws)-1](last)
	for i := len(mws) - 2; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}
