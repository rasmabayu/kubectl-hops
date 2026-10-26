package trace

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// ParseHost turns what a user pastes into --host into a bare host name. It
// accepts a plain host ("shop.example.com"), a host with port, or a full URL
// ("https://shop.example.com/api"), in which case the URL path is returned too
// so callers can use it when --path was not given.
func ParseHost(raw string) (host, path string, err error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", "", nil
	}
	if !strings.Contains(s, "://") {
		// Let url.Parse split host, port and path for us.
		s = "//" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", "", fmt.Errorf("invalid --host %q: %w", raw, err)
	}
	host = u.Hostname()
	if host == "" {
		return "", "", fmt.Errorf("invalid --host %q: no host name", raw)
	}
	if net.ParseIP(host) != nil {
		return "", "", fmt.Errorf("invalid --host %q: Ingress rules match host names, not IP addresses", raw)
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	return host, u.EscapedPath(), nil
}
