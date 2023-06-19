package service

import (
	"github.com/livekit/protocol/logger"
	"net"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
)

func handleError(w http.ResponseWriter, status int, err error, keysAndValues ...interface{}) {
	keysAndValues = append(keysAndValues, "status", status)
	logger.GetLogger().WithCallDepth(1).Warnw("error handling request", err, keysAndValues...)
	w.WriteHeader(status)
	_, _ = w.Write([]byte(err.Error()))
}

func boolValue(s string) bool {
	return s == "1" || s == "true"
}

func cacheDir() (dir string) {
	if u, _ := user.Current(); u != nil {
		dir = filepath.Join(os.TempDir(), "cache-golang-autocert-"+u.Username)
		if err := os.MkdirAll(dir, 0700); err == nil {
			return dir
		}
	}
	return ""
}

func IsValidDomain(domain string) bool {
	domainRegexp := regexp.MustCompile(`^(?i)[a-z0-9-]+(\.[a-z0-9-]+)+\.?$`)
	return domainRegexp.MatchString(domain)
}

func GetClientIP(r *http.Request) string {
	// CF proxy typically is first thing the user reaches
	if ip := r.Header.Get("CF-Connecting-IP"); ip != "" {
		return ip
	}
	if ip := r.Header.Get("X-Forwarded-For"); ip != "" {
		return ip
	}
	if ip := r.Header.Get("X-Real-IP"); ip != "" {
		return ip
	}
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	return ip
}
