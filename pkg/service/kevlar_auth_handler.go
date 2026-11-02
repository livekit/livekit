// Copyright 2026 LiveKit, Inc. and Flipkart FK fork.
//
// FK patch: pre-join Kevlar AT validation against icap-gateway /v1/validate.
// When kevlar_auth_handler.url is set, RTC joins (non-agent) must supply the AT
// (preferred: HTTP headers on the /rtc upgrade; fallback: join attrs / JWT attrs).
// The SFU GETs the gateway and rejects the WS on failure.

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
	// gatewayTenantHeader is the canonical tenant header for icap-gateway /v1/validate.
	gatewayTenantHeader = "x-tenant-id"
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

// authValidateResponse matches icap-gateway /v1/validate JSON (AuthenticateResponse).
// Current shape uses userId; accountId kept as a fallback for older deployments.
type authValidateResponse struct {
	UserID    string `json:"userId"`
	AccountID string `json:"accountId"`
	Tenant    string `json:"tenant"`
}

func (b authValidateResponse) resolvedAccountID() string {
	if b.UserID != "" {
		return b.UserID
	}
	return b.AccountID
}

// ValidateConnect runs the gateway check and strips the AT from join/JWT attributes
// so it is not stored on the participant. Agents are skipped (no Kevlar AT).
//
// Credential resolution order for at / tenant:
//  1. HTTP request headers on the /rtc upgrade (preferred — not query attrs)
//  2. Join participant attributes
//  3. LiveKit JWT claim attributes
//
// Tenant headers accepted: x-tenant-id (gateway canonical) and tenantAttribute (default "tenant").
func (h *KevlarAuthHandler) ValidateConnect(
	ctx context.Context,
	lgr logger.Logger,
	claimsIdentity string,
	kind livekit.ParticipantInfo_Kind,
	reqHeader http.Header,
	attributes map[string]string,
	tokenAttributes map[string]string,
) error {
	if h == nil {
		return nil
	}
	if kind == livekit.ParticipantInfo_AGENT {
		return nil
	}

	at, atSrc := h.resolveATWithSource(reqHeader, attributes, tokenAttributes)
	tenant, tenantSrc := h.resolveTenantWithSource(reqHeader, attributes, tokenAttributes)
	// Log received headers/attrs for join debugging — never log raw AT.
	lgr.Infow("kevlar_auth_handler connect credentials",
		"identity", claimsIdentity,
		"headerAtPresent", headerGet(reqHeader, h.tokenAttribute) != "",
		"headerAtLen", len(headerGet(reqHeader, h.tokenAttribute)),
		"headerXTenantId", headerGet(reqHeader, gatewayTenantHeader),
		"headerTenant", headerGet(reqHeader, h.tenantAttribute),
		"attrAtPresent", attributes != nil && attributes[h.tokenAttribute] != "",
		"attrTenant", mapGet(attributes, h.tenantAttribute),
		"jwtAttrAtPresent", tokenAttributes != nil && tokenAttributes[h.tokenAttribute] != "",
		"jwtAttrTenant", mapGet(tokenAttributes, h.tenantAttribute),
		"resolvedAtSource", atSrc,
		"resolvedAtLen", len(at),
		"resolvedTenant", tenant,
		"resolvedTenantSource", tenantSrc,
	)

	if at == "" {
		return ErrKevlarAuthHandlerMissingToken
	}

	if tenant == "" {
		return ErrKevlarAuthHandlerMissingTenant
	}

	accountID, err := h.validate(ctx, at, tenant)
	if err != nil {
		lgr.Infow("kevlar_auth_handler rejected join", "error", err, "identity", claimsIdentity, "tenant", tenant)
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

func (h *KevlarAuthHandler) resolveAT(
	reqHeader http.Header,
	attributes, tokenAttributes map[string]string,
) string {
	v, _ := h.resolveATWithSource(reqHeader, attributes, tokenAttributes)
	return v
}

func (h *KevlarAuthHandler) resolveATWithSource(
	reqHeader http.Header,
	attributes, tokenAttributes map[string]string,
) (string, string) {
	if v := headerGet(reqHeader, h.tokenAttribute); v != "" {
		return v, "header"
	}
	if attributes != nil {
		if v := attributes[h.tokenAttribute]; v != "" {
			return v, "join_attr"
		}
	}
	if tokenAttributes != nil {
		if v := tokenAttributes[h.tokenAttribute]; v != "" {
			return v, "jwt_attr"
		}
	}
	return "", "missing"
}

func (h *KevlarAuthHandler) resolveTenant(
	reqHeader http.Header,
	attributes, tokenAttributes map[string]string,
) string {
	v, _ := h.resolveTenantWithSource(reqHeader, attributes, tokenAttributes)
	return v
}

func (h *KevlarAuthHandler) resolveTenantWithSource(
	reqHeader http.Header,
	attributes, tokenAttributes map[string]string,
) (string, string) {
	if v := headerGet(reqHeader, gatewayTenantHeader); v != "" {
		return v, "header_x-tenant-id"
	}
	if v := headerGet(reqHeader, h.tenantAttribute); v != "" {
		return v, "header_" + h.tenantAttribute
	}
	if attributes != nil {
		if v := attributes[h.tenantAttribute]; v != "" {
			return v, "join_attr"
		}
	}
	if tokenAttributes != nil {
		if v := tokenAttributes[h.tenantAttribute]; v != "" {
			return v, "jwt_attr"
		}
	}
	return "", "missing"
}

func headerGet(h http.Header, key string) string {
	if h == nil {
		return ""
	}
	return h.Get(key)
}

func mapGet(m map[string]string, key string) string {
	if m == nil {
		return ""
	}
	return m[key]
}

func (h *KevlarAuthHandler) validate(ctx context.Context, at, tenant string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("at", at)
	req.Header.Set(gatewayTenantHeader, tenant)

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
	accountID := body.resolvedAccountID()
	if accountID == "" {
		return "", errors.New("empty userId/accountId")
	}
	return accountID, nil
}
