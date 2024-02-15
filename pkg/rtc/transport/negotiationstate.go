package transport

import "fmt"

type NegotiationState int

const (
	NegotiationStateNone NegotiationState = iota
	// waiting for remote description
	NegotiationStateRemote
	// need to Negotiate again
	NegotiationStateRetry
)

func (n NegotiationState) String() string {
	switch n {
	case NegotiationStateNone:
		return "NONE"
	case NegotiationStateRemote:
		return "WAITING_FOR_REMOTE"
	case NegotiationStateRetry:
		return "RETRY"
	default:
		return fmt.Sprintf("%d", int(n))
	}
}
