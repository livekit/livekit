package service

import (
	"context"
	"strings"

	"github.com/twitchtv/twirp"

	"github.com/livekit/protocol/utils/guid"
)

type egressIDKey struct{}

func TwirpEgressID() *twirp.ServerHooks {
	return &twirp.ServerHooks{
		RequestRouted: func(ctx context.Context) (context.Context, error) {
			// generate egressID for start egress methods for tracing egress failure before it reaches egress service.
			if isStartEgressMethod(ctx) {
				egressID := guid.New(guid.EgressPrefix)
				ctx = WithEgressID(ctx, egressID)
				AppendLogFields(ctx, "egressID", egressID)
			}
			return ctx, nil
		},
	}
}

func WithEgressID(ctx context.Context, egressID string) context.Context {
	return context.WithValue(ctx, egressIDKey{}, egressID)
}

func EgressID(ctx context.Context) (string, bool) {
	if egressID, ok := ctx.Value(egressIDKey{}).(string); ok && egressID != "" {
		return egressID, true
	}
	return "", false
}

func isStartEgressMethod(ctx context.Context) bool {
	if methodName, ok := twirp.MethodName(ctx); ok && strings.HasPrefix(methodName, "Start") && strings.HasSuffix(methodName, "Egress") {
		return true
	}
	return false
}
