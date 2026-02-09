package service

import (
	"context"
	"strings"

	"github.com/twitchtv/twirp"

	"github.com/livekit/protocol/utils/guid"
	"github.com/livekit/psrpc/pkg/metadata"
)

const egressIDKey = "egressID"

func TwirpEgressID() *twirp.ServerHooks {
	return &twirp.ServerHooks{
		RequestRouted: func(ctx context.Context) (context.Context, error) {
			// generate egressID for all Start*Egress methods
			if methodName, ok := twirp.MethodName(ctx); ok && strings.HasPrefix(methodName, "Start") && strings.HasSuffix(methodName, "Egress") {
				egressID := guid.New(guid.EgressPrefix)
				ctx = WithEgressID(ctx, egressID)
				AppendLogFields(ctx, "egressID", egressID)
			}
			return ctx, nil
		},
	}
}

func WithEgressID(ctx context.Context, egressID string) context.Context {
	return metadata.AppendMetadataToOutgoingContext(ctx, egressIDKey, egressID)
}

func EgressID(ctx context.Context) (string, bool) {
	head := metadata.IncomingHeader(ctx)
	if head == nil {
		return "", false
	}
	id, ok := head.Metadata[egressIDKey]
	return id, ok
}
