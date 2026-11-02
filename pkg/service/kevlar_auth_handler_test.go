package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/stretchr/testify/require"

	"github.com/livekit/livekit-server/pkg/config"
)

func TestNewKevlarAuthHandler_DisabledWhenURLEmpty(t *testing.T) {
	require.Nil(t, NewKevlarAuthHandler(config.KevlarAuthHandlerConfig{}))
}

func TestKevlarAuthHandler_ValidateConnect_FromHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodGet, r.Method)
		require.Equal(t, "kevlar-at", r.Header.Get("at"))
		require.Equal(t, "FLIPKART", r.Header.Get("x-tenant-id"))
		_ = json.NewEncoder(w).Encode(map[string]string{"tenant": "BRAND_COMM", "userId": "acc-1"})
	}))
	defer srv.Close()

	h := NewKevlarAuthHandler(config.KevlarAuthHandlerConfig{URL: srv.URL})
	hdr := http.Header{}
	hdr.Set("at", "kevlar-at")
	hdr.Set("x-tenant-id", "FLIPKART")
	err := h.ValidateConnect(context.Background(), logger.GetLogger(), "user-acc-1", livekit.ParticipantInfo_STANDARD, hdr, nil, nil)
	require.NoError(t, err)
}

func TestKevlarAuthHandler_ValidateConnect_HeadersPreferredOverAttrs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "from-header", r.Header.Get("at"))
		require.Equal(t, "FLIPKART", r.Header.Get("x-tenant-id"))
		_ = json.NewEncoder(w).Encode(map[string]string{"userId": "acc-1"})
	}))
	defer srv.Close()

	h := NewKevlarAuthHandler(config.KevlarAuthHandlerConfig{URL: srv.URL})
	hdr := http.Header{}
	hdr.Set("at", "from-header")
	hdr.Set("tenant", "FLIPKART")
	attrs := map[string]string{"at": "from-attrs", "tenant": "WRONG"}
	err := h.ValidateConnect(context.Background(), logger.GetLogger(), "user-acc-1", livekit.ParticipantInfo_STANDARD, hdr, attrs, nil)
	require.NoError(t, err)
	_, hasAt := attrs["at"]
	require.False(t, hasAt, "at still stripped from attrs if present")
}

func TestKevlarAuthHandler_ValidateConnect_LegacyAccountIdField(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"accountId": "acc-1"})
	}))
	defer srv.Close()

	h := NewKevlarAuthHandler(config.KevlarAuthHandlerConfig{URL: srv.URL})
	attrs := map[string]string{"at": "kevlar-at", "tenant": "FLIPKART"}
	err := h.ValidateConnect(context.Background(), logger.GetLogger(), "user-acc-1", livekit.ParticipantInfo_STANDARD, nil, attrs, nil)
	require.NoError(t, err)
}

func TestKevlarAuthHandler_ValidateConnect_SuccessAndStrip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodGet, r.Method)
		require.Equal(t, "kevlar-at", r.Header.Get("at"))
		require.Equal(t, "FLIPKART", r.Header.Get("x-tenant-id"))
		_ = json.NewEncoder(w).Encode(map[string]string{"tenant": "BRAND_COMM", "userId": "acc-1"})
	}))
	defer srv.Close()

	h := NewKevlarAuthHandler(config.KevlarAuthHandlerConfig{URL: srv.URL})
	attrs := map[string]string{"at": "kevlar-at", "tenant": "FLIPKART", "keep": "1"}
	err := h.ValidateConnect(context.Background(), logger.GetLogger(), "user-acc-1", livekit.ParticipantInfo_STANDARD, nil, attrs, nil)
	require.NoError(t, err)
	_, hasAt := attrs["at"]
	require.False(t, hasAt)
	require.Equal(t, "FLIPKART", attrs["tenant"])
	require.Equal(t, "1", attrs["keep"])
}

func TestKevlarAuthHandler_ValidateConnect_MissingToken(t *testing.T) {
	h := NewKevlarAuthHandler(config.KevlarAuthHandlerConfig{URL: "http://example.invalid"})
	err := h.ValidateConnect(context.Background(), logger.GetLogger(), "user-acc", livekit.ParticipantInfo_STANDARD, nil, map[string]string{"tenant": "FLIPKART"}, nil)
	require.ErrorIs(t, err, ErrKevlarAuthHandlerMissingToken)
}

func TestKevlarAuthHandler_ValidateConnect_MissingTenant(t *testing.T) {
	h := NewKevlarAuthHandler(config.KevlarAuthHandlerConfig{URL: "http://example.invalid"})
	err := h.ValidateConnect(context.Background(), logger.GetLogger(), "user-acc", livekit.ParticipantInfo_STANDARD, nil, map[string]string{"at": "t"}, nil)
	require.ErrorIs(t, err, ErrKevlarAuthHandlerMissingTenant)
}

func TestKevlarAuthHandler_ValidateConnect_GatewayReject(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	h := NewKevlarAuthHandler(config.KevlarAuthHandlerConfig{URL: srv.URL})
	err := h.ValidateConnect(context.Background(), logger.GetLogger(), "user-acc", livekit.ParticipantInfo_STANDARD, nil, map[string]string{"at": "bad", "tenant": "FLIPKART"}, nil)
	require.ErrorIs(t, err, ErrKevlarAuthHandlerRejected)
}

func TestKevlarAuthHandler_ValidateConnect_SkipsAgent(t *testing.T) {
	h := NewKevlarAuthHandler(config.KevlarAuthHandlerConfig{URL: "http://example.invalid"})
	err := h.ValidateConnect(context.Background(), logger.GetLogger(), "agent", livekit.ParticipantInfo_AGENT, nil, map[string]string{}, nil)
	require.NoError(t, err)
}

func TestKevlarAuthHandler_ValidateConnect_IdentityMustBeUserPrefix(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"tenant": "BRAND_COMM",
			"userId": "ACC9F1A6A068EAB4467993ADE14271DA383Y",
		})
	}))
	defer srv.Close()

	h := NewKevlarAuthHandler(config.KevlarAuthHandlerConfig{URL: srv.URL})
	attrs := map[string]string{"at": "t", "tenant": "FLIPKART"}

	err := h.ValidateConnect(context.Background(), logger.GetLogger(),
		"user-ACC9F1A6A068EAB4467993ADE14271DA383Y", livekit.ParticipantInfo_STANDARD, nil, attrs, nil)
	require.NoError(t, err)

	err = h.ValidateConnect(context.Background(), logger.GetLogger(),
		"ACC9F1A6A068EAB4467993ADE14271DA383Y", livekit.ParticipantInfo_STANDARD, nil,
		map[string]string{"at": "t", "tenant": "FLIPKART"}, nil)
	require.ErrorIs(t, err, ErrKevlarAuthHandlerIdentity)

	err = h.ValidateConnect(context.Background(), logger.GetLogger(),
		"user-other", livekit.ParticipantInfo_STANDARD, nil,
		map[string]string{"at": "t", "tenant": "FLIPKART"}, nil)
	require.ErrorIs(t, err, ErrKevlarAuthHandlerIdentity)
}
