// Copyright 2026 LiveKit, Inc. and Flipkart FK fork.
//
// FK patch: pre-join Kevlar AT validation against icap-gateway /v1/validate.
// When kevlar_auth_handler.url is set, RTC joins (non-agent) must supply the AT in
// participant attributes; the SFU GETs the gateway and rejects the WS on failure.

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/config"
)

const (
	defaultKevlarAuthHandlerTimeout         = 2 * time.Second
	defaultKevlarAuthHandlerTokenAttribute  = "at"
	defaultKevlarAuthHandlerTenantAttribute = "tenant"
	// userIdentityPrefix matches room-management LiveKit identity: user-{accountId}.
	userIdentityPrefix = "user-"
)

var (
	ErrKevlarAuthHandlerMissingToken  = errors.New("missing kevlar access token for kevlar_auth_handler")
	ErrKevlarAuthHandlerMissingTenant = errors.New("missing tenant for kevlar_auth_handler")
	ErrKevlarAuthHandlerRejected      = errors.New("kevlar_auth_handler rejected connection")
	ErrKevlarAuthHandlerIdentity      = errors.New("kevlar_auth_handler identity is not user-{accountId}")
)

// KevlarAuthHandler validates a Kevlar AT via icap-gateway before RTC join.
type KevlarAuthHandler struct {
	url             string
	tokenAttribute  string
	tenantAttribute string
	httpClient      *http.Client
}

func NewKevlarAuthHandler(cfg config.KevlarAuthHandlerConfig) *KevlarAuthHandler {
	if cfg.URL == "" {
		return nil
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultKevlarAuthHandlerTimeout
	}
	tokenAttr := cfg.TokenAttribute
	if tokenAttr == "" {
		tokenAttr = defaultKevlarAuthHandlerTokenAttribute
	}
	tenantAttr := cfg.TenantAttribute
	if tenantAttr == "" {
		tenantAttr = defaultKevlarAuthHandlerTenantAttribute
	}
	return &KevlarAuthHandler{
		url:             cfg.URL,
		tokenAttribute:  tokenAttr,
		tenantAttribute: tenantAttr,
		httpClient:      &http.Client{Timeout: timeout},
	}
}

type authValidateResponse struct {
	AccountID string `json:"accountId"`
}

// ValidateConnect runs the gateway check and strips the AT from attributes so
// it is not stored on the participant. Agents are skipped (no Kevlar AT).
func (h *KevlarAuthHandler) ValidateConnect(
	ctx context.Context,
	lgr logger.Logger,
	claimsIdentity string,
	kind livekit.ParticipantInfo_Kind,
	attributes map[string]string,
	tokenAttributes map[string]string,
) error {
	if h == nil {
		return nil
	}
	if kind == livekit.ParticipantInfo_AGENT {
		return nil
	}

	at := ""
	if attributes != nil {
		at = attributes[h.tokenAttribute]
	}
	if at == "" && tokenAttributes != nil {
		at = tokenAttributes[h.tokenAttribute]
	}
	if at == "" {
		return ErrKevlarAuthHandlerMissingToken
	}

	tenant := ""
	if attributes != nil {
		tenant = attributes[h.tenantAttribute]
	}
	if tenant == "" && tokenAttributes != nil {
		tenant = tokenAttributes[h.tenantAttribute]
	}
	if tenant == "" {
		return ErrKevlarAuthHandlerMissingTenant
	}

	accountID, err := h.validate(ctx, at, tenant)
	if err != nil {
		lgr.Infow("kevlar_auth_handler rejected join", "error", err, "identity", claimsIdentity)
		return fmt.Errorf("%w: %v", ErrKevlarAuthHandlerRejected, err)
	}

	if attributes != nil {
		delete(attributes, h.tokenAttribute)
	}
	if tokenAttributes != nil {
		delete(tokenAttributes, h.tokenAttribute)
	}

	// LiveKit identity from room-management is always user-{accountId}.
	if claimsIdentity != userIdentityPrefix+accountID {
		return ErrKevlarAuthHandlerIdentity
	}
	return nil
}

func (h *KevlarAuthHandler) validate(ctx context.Context, at, tenant string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.url, nil)
	if err != nil {
		return "", err
	}
	// Participant attrs use "at" + "tenant"; gateway HTTP expects "at" + "x-tenant-id".
	req.Header.Set("at", at)
	req.Header.Set("x-tenant-id", tenant)

	resp, err := h.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status=%d", resp.StatusCode)
	}

	var body authValidateResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}
	if body.AccountID == "" {
		return "", errors.New("empty accountId")
	}
	return body.AccountID, nil
}
