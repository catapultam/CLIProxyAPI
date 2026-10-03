// Package marketplace serves the Claude Code plugins embedded in this
// binary as a plugin marketplace (see code.claude.com/docs/en/plugins), so
// client machines can install them straight from the proxy.
package marketplace

import (
	"net/http"
	"os"
	"strings"
)

// publicURLEnv names the externally reachable https base URL of the proxy,
// e.g. "https://cakebox.wyrm-cat.ts.net:8444". Set it when clients reach the
// API over plain http on one port and the marketplace over https on another.
const publicURLEnv = "CPA_PUBLIC_URL"

// BaseURL returns the externally reachable https base URL for an incoming
// request. CPA_PUBLIC_URL wins when set. Otherwise it prefers the first value
// of X-Forwarded-Host (set by a reverse proxy) and falls back to the request's
// own Host. The scheme is always https: Claude Code refuses archive URLs
// that aren't.
func BaseURL(r *http.Request) string {
	if public := strings.TrimRight(strings.TrimSpace(os.Getenv(publicURLEnv)), "/"); public != "" {
		return public
	}
	host := r.Header.Get("X-Forwarded-Host")
	if host == "" {
		host = r.Host
	} else if idx := strings.IndexByte(host, ','); idx >= 0 {
		host = host[:idx]
	}
	return "https://" + strings.TrimSpace(host)
}
