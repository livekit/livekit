package service

import (
	"net/http"
	"regexp"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/logger"
	livekit "github.com/livekit/protocol/proto"
)

func handleError(w http.ResponseWriter, status int, msg string) {
	// GetLogger already with extra depth 1
	logger.GetLogger().V(1).Info("error handling request", "error", msg, "status", status)
	w.WriteHeader(status)
	_, _ = w.Write([]byte(msg))
}

func boolValue(s string) bool {
	return s == "1" || s == "true"
}

func IsValidDomain(domain string) bool {
	domainRegexp := regexp.MustCompile(`^(?i)[a-z0-9-]+(\.[a-z0-9-]+)+\.?$`)
	return domainRegexp.MatchString(domain)
}

func permissionFromGrant(claim *auth.VideoGrant) *livekit.ParticipantPermission {
	p := &livekit.ParticipantPermission{
		CanSubscribe:   true,
		CanPublish:     true,
		CanPublishData: true,
	}
	if claim.CanPublish != nil {
		p.CanPublish = *claim.CanPublish
	}
	if claim.CanSubscribe != nil {
		p.CanSubscribe = *claim.CanSubscribe
	}
	if claim.CanPublishData != nil {
		p.CanPublishData = *claim.CanPublishData
	}
	return p
}
