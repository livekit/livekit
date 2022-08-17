package service

import (
	"net/http"
	"regexp"

	"github.com/livekit/protocol/logger"
)

func handleError(w http.ResponseWriter, status int, err error, keysAndValues ...interface{}) {
	keysAndValues = append(keysAndValues, "status", status)
	logger.Warnw("error handling request", err, keysAndValues...)
	w.WriteHeader(status)
	_, _ = w.Write([]byte(err.Error()))
}

func boolValue(s string) bool {
	return s == "1" || s == "true"
}

func IsValidDomain(domain string) bool {
	domainRegexp := regexp.MustCompile(`^(?i)[a-z0-9-]+(\.[a-z0-9-]+)+\.?$`)
	return domainRegexp.MatchString(domain)
}
