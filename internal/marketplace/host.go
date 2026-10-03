// Package marketplace serves the Claude Code plugins embedded in this
// binary as a plugin marketplace (see code.claude.com/docs/en/plugins), so
// client machines can install them straight from the proxy.
package marketplace

import (
	"net/http"
	"strings"
)

// BaseURL returns the externally reachable https base URL for an incoming
// request, e.g. "https://proxy.tailnet:8317". It prefers the first value of
// X-Forwarded-Host (set by a reverse proxy) and falls back to the request's
// own Host. The scheme is always https: Claude Code refuses archive URLs
// that aren't, and this proxy is only ever reached over the tailnet or
// behind a TLS-terminating proxy.
func BaseURL(r *http.Request) string {
	host := r.Header.Get("X-Forwarded-Host")
	if host == "" {
		host = r.Host
	} else if idx := strings.IndexByte(host, ','); idx >= 0 {
		host = host[:idx]
	}
	return "https://" + strings.TrimSpace(host)
}
