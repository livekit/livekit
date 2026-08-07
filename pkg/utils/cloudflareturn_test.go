package utils

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestFetchCloudflareIceConfig(t *testing.T) {
	const jsonResp = `{
	  "iceServers": [{
	    "urls": [
	      "stun:stun.cloudflare.com:3478",
	      "stun:stun.cloudflare.com:53",
	      "turn:turn.cloudflare.com:3478?transport=udp",
	      "turn:turn.cloudflare.com:53?transport=udp",
	      "turn:turn.cloudflare.com:3478?transport=tcp",
	      "turn:turn.cloudflare.com:80?transport=tcp",
	      "turns:turn.cloudflare.com:5349?transport=tcp",
	      "turns:turn.cloudflare.com:443?transport=tcp"
	    ],
	    "username":   "bc91…e3e",
	    "credential": "ebd7…4a0"
	  }]
	}`

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("bad Authorization header: %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(jsonResp))
	}))
	defer ts.Close()

	// Override base URL *just for this test*
	oldBase := rtcBaseURL
	rtcBaseURL = ts.URL
	defer func() { rtcBaseURL = oldBase }()

	got, err := FetchCloudflareTurnIceConfig(context.Background(), "test-key", "test-token")
	if err != nil {
		t.Fatalf("FetchCloudflareTurnIceConfig returned error: %v", err)
	}

	want := &CloudFlareIceConfig{
		IceServers: []CloudFlareIceServer{{
			URLs: []string{
				"stun:stun.cloudflare.com:3478",
				"stun:stun.cloudflare.com:53",
				"turn:turn.cloudflare.com:3478?transport=udp",
				"turn:turn.cloudflare.com:53?transport=udp",
				"turn:turn.cloudflare.com:3478?transport=tcp",
				"turn:turn.cloudflare.com:80?transport=tcp",
				"turns:turn.cloudflare.com:5349?transport=tcp",
				"turns:turn.cloudflare.com:443?transport=tcp",
			},
			Username:   "bc91…e3e",
			Credential: "ebd7…4a0",
		}},
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mismatch:\n got  %+v\n want %+v", got, want)
	}
}

func TestFetchCloudflareIceConfig_Non200(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer ts.Close()

	oldBase := rtcBaseURL
	rtcBaseURL = ts.URL
	defer func() { rtcBaseURL = oldBase }()

	if _, err := FetchCloudflareTurnIceConfig(context.Background(), "x", "y"); err == nil {
		t.Fatal("expected error for 500 response, got nil")
	}
}
