// Copyright 2026 LiveKit, Inc. and Flipkart FK fork.
//
// icap-gateway authenticates the Kevlar AT on /rtc and injects x-account-id.
// This check ensures the join JWT identity is user-{that account id}.

package service

import (
	"errors"
	"net/http"

	"github.com/livekit/protocol/livekit"
)

const (
	gatewayAccountIDHeader = "x-account-id"
	userIdentityPrefix     = "user-"
)

var (
	ErrMissingGatewayAccountID = errors.New("missing x-account-id from gateway")
	ErrGatewayAccountMismatch  = errors.New("x-account-id does not match join identity")
)

// ensureGatewayAccountMatchesIdentity requires x-account-id from the gateway and
// that it equals the accountId in identity user-{accountId}.
// Agents are skipped (no Kevlar AT / gateway account header).
func ensureGatewayAccountMatchesIdentity(
	reqHeader http.Header,
	claimsIdentity string,
	kind livekit.ParticipantInfo_Kind,
) error {
	if kind == livekit.ParticipantInfo_AGENT {
		return nil
	}
	accountID := ""
	if reqHeader != nil {
		accountID = reqHeader.Get(gatewayAccountIDHeader)
	}
	if accountID == "" {
		return ErrMissingGatewayAccountID
	}
	if claimsIdentity != userIdentityPrefix+accountID {
		return ErrGatewayAccountMismatch
	}
	return nil
}
