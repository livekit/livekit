package utils

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

var rtcBaseURL = "https://rtc.live.cloudflare.com"
var client = &http.Client{
	Timeout: 10 * time.Second,
}

type CloudFlareIceConfig struct {
	IceServers []CloudFlareIceServer `json:"iceServers"`
}

type CloudFlareIceServer struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username"`
	Credential string   `json:"credential"`
}

func FetchCloudflareTurnIceConfig(ctx context.Context, keyID, apiToken string) (*CloudFlareIceConfig, error) {
	url := fmt.Sprintf("%s/v1/turn/keys/%s/credentials/generate-ice-servers", rtcBaseURL, keyID)
	payload := struct {
		TTL int `json:"ttl"`
	}{TTL: int(iceConfigTTLMin.Seconds())}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", "Bearer "+apiToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("unexpected status: %s, body: %v, err:%v,", resp.Status, resp, err)
	}

	var cfg CloudFlareIceConfig
	if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}
