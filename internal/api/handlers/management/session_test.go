package management

import (
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/mgmtauth"
)

// newTestEngine wires up the same route shape server_management_v8.go
// registers, scoped to this package's tests: session routes outside
// Middleware(), account routes (and representative /v0 and /v8 routes)
// behind it.
func newTestEngine(h *Handler) *gin.Engine {
	engine := gin.New()

	session := engine.Group("/v8/management/session")
	session.GET("/status", h.GetSessionStatus)
	session.POST("/login", h.PostSessionLogin)
	session.POST("/passkey/begin", h.PostSessionPasskeyBegin)
	session.POST("/passkey/finish", h.PostSessionPasskeyFinish)
	session.POST("/logout", h.PostSessionLogout)

	account := engine.Group("/v8/management/account")
	account.Use(h.Middleware())
	account.GET("", h.GetAccount)
	account.PUT("", h.PutAccount)
	account.PUT("/passkey-settings", h.PutAccountPasskeySettings)
	account.POST("/passkeys/begin", h.PostAccountPasskeysBegin)
	account.POST("/passkeys/finish", h.PostAccountPasskeysFinish)
	account.PATCH("/passkeys/:id", h.PatchAccountPasskey)
	account.DELETE("/passkeys/:id", h.DeleteAccountPasskey)
	account.POST("/sign-out-all", h.PostAccountSignOutAll)

	v0 := engine.Group("/v0/management")
	v0.Use(h.Middleware())
	v0.GET("/config", h.GetConfig)

	v8 := engine.Group("/v8/management")
	v8.Use(h.Middleware())
	v8.GET("/probe", func(c *gin.Context) { c.Status(http.StatusOK) })
	v8.GET("/config", h.ConfigV8)
	v8.GET("/config.yaml", h.ConfigV8)

	return engine
}

const testAccountPassword = "correct-horse-battery-staple"

func newTestHandlerBase(t *testing.T, clock mgmtauth.Clock) *Handler {
	t.Helper()
	path := writeTestConfigFile(t)
	return &Handler{
		cfg:               &config.Config{},
		configFilePath:    path,
		failedAttempts:    make(map[string]*attemptInfo),
		loginStore:        mgmtauth.NewStore(path),
		loginThrottle:     mgmtauth.NewThrottle(clock),
		passkeyCeremonies: mgmtauth.NewCeremonyCache(clock),
		clock:             clock,
	}
}

// newAccountHandler builds a Handler with a pre-configured username/password
// account (no passkeys), driven by clock for deterministic tests.
func newAccountHandler(t *testing.T, clock mgmtauth.Clock) *Handler {
	t.Helper()
	h := newTestHandlerBase(t, clock)
	hash, err := mgmtauth.HashPassword(testAccountPassword)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	secret, err := generateAccountSecret()
	if err != nil {
		t.Fatalf("generateAccountSecret: %v", err)
	}
	handle, err := generateAccountSecret()
	if err != nil {
		t.Fatalf("generateAccountSecret: %v", err)
	}
	if _, err := h.loginStore.Mutate(func(*mgmtauth.Account) (*mgmtauth.Account, error) {
		return &mgmtauth.Account{
			Username:      "admin",
			PasswordHash:  hash,
			SessionSecret: secret,
			UserHandle:    handle,
		}, nil
	}); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	return h
}

// keyOnlyHandler builds a Handler with a management key but no login
// account yet, for first-setup PUT /account tests.
func keyOnlyHandler(t *testing.T) *Handler {
	t.Helper()
	h := newTestHandlerBase(t, mgmtauth.SystemClock{})
	h.envSecret = "test-secret"
	h.allowRemoteOverride = true
	return h
}

func doRequest(engine *gin.Engine, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	return doRequestFrom(engine, method, path, body, headers, "127.0.0.1:12345")
}

func doRequestFrom(engine *gin.Engine, method, path, body string, headers map[string]string, remoteAddr string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.RemoteAddr = remoteAddr
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	engine.ServeHTTP(rec, req)
	return rec
}

func loginJSON(username, password string) string {
	b, _ := json.Marshal(map[string]string{"username": username, "password": password})
	return string(b)
}

func sessionCookieFrom(rec *httptest.ResponseRecorder) (string, string) {
	for _, c := range rec.Result().Cookies() {
		if c.Name == SessionCookieName {
			return c.Name + "=" + c.Value, c.Value
		}
	}
	return "", ""
}

// TestPostSessionLoginNoSecureForZeroValueTLSState covers the real server:
// its bufferedConn implements ConnectionState, so plain HTTP requests carry a
// non-nil, zero-value Request.TLS that must not count as HTTPS.
func TestPostSessionLoginNoSecureForZeroValueTLSState(t *testing.T) {
	h := newAccountHandler(t, mgmtauth.SystemClock{})
	engine := newTestEngine(h)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v8/management/session/login", strings.NewReader(loginJSON("admin", testAccountPassword)))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	req.TLS = &tls.ConnectionState{}
	engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if setCookie := rec.Header().Get("Set-Cookie"); strings.Contains(setCookie, "Secure") {
		t.Fatalf("Set-Cookie = %q, expected no Secure for a zero-value TLS state", setCookie)
	}
}

func TestPostSessionLoginSuccess(t *testing.T) {
	h := newAccountHandler(t, mgmtauth.SystemClock{})
	engine := newTestEngine(h)

	rec := doRequest(engine, http.MethodPost, "/v8/management/session/login", loginJSON("admin", testAccountPassword), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !strings.HasPrefix(resp.Token, mgmtauth.TokenPrefix) {
		t.Fatalf("token = %q, want cpas_ prefix", resp.Token)
	}
	if _, err := time.Parse(time.RFC3339, resp.ExpiresAt); err != nil {
		t.Fatalf("expires_at = %q is not RFC3339: %v", resp.ExpiresAt, err)
	}
	cookieHeader, _ := sessionCookieFrom(rec)
	if cookieHeader == "" {
		t.Fatal("expected a session cookie to be set")
	}
	setCookie := rec.Header().Get("Set-Cookie")
	if !strings.Contains(setCookie, "HttpOnly") || !strings.Contains(setCookie, "SameSite=Strict") || !strings.Contains(setCookie, "Path=/v8/management") {
		t.Fatalf("Set-Cookie = %q, missing expected attributes", setCookie)
	}
	if strings.Contains(setCookie, "Secure") {
		t.Fatalf("Set-Cookie = %q, expected no Secure attribute over plain HTTP", setCookie)
	}
}

func TestPostSessionLoginSecureCookieOverHTTPSOrigin(t *testing.T) {
	h := newAccountHandler(t, mgmtauth.SystemClock{})
	engine := newTestEngine(h)

	rec := doRequest(engine, http.MethodPost, "/v8/management/session/login", loginJSON("admin", testAccountPassword), map[string]string{
		"Origin": "https://cakebox.example.com",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	setCookie := rec.Header().Get("Set-Cookie")
	if !strings.Contains(setCookie, "Secure") {
		t.Fatalf("Set-Cookie = %q, expected Secure when Origin is https", setCookie)
	}
}

func TestPostSessionLoginSecureCookieOverHTTPSReferer(t *testing.T) {
	h := newAccountHandler(t, mgmtauth.SystemClock{})
	engine := newTestEngine(h)

	rec := doRequest(engine, http.MethodPost, "/v8/management/session/login", loginJSON("admin", testAccountPassword), map[string]string{
		"Referer": "https://cakebox.example.com/login",
	})
	setCookie := rec.Header().Get("Set-Cookie")
	if !strings.Contains(setCookie, "Secure") {
		t.Fatalf("Set-Cookie = %q, expected Secure when Referer is https", setCookie)
	}
}

// TestSlidingRefreshKeepsSecureOnHTTPSPasskeyOrigin covers a GET that
// carries neither Origin nor Referer (as a direct navigation would) against
// an https passkey origin whose host:port matches the request Host: the
// sliding refresh it triggers must still set Secure.
func TestSlidingRefreshKeepsSecureOnHTTPSPasskeyOrigin(t *testing.T) {
	clock := newMockClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	h := newAccountHandler(t, clock)
	if _, err := h.loginStore.Mutate(func(acct *mgmtauth.Account) (*mgmtauth.Account, error) {
		acct.PasskeyOrigins = []string{"https://cakebox.example.com:8443"}
		return acct, nil
	}); err != nil {
		t.Fatalf("seed passkey origins: %v", err)
	}
	engine := newTestEngine(h)

	loginRec := doRequest(engine, http.MethodPost, "/v8/management/session/login", loginJSON("admin", testAccountPassword), nil)
	cookieHeader, _ := sessionCookieFrom(loginRec)

	// Advance past the halfway point of the lifetime so Middleware() slides
	// the session forward on the next request.
	clock.Advance(mgmtauth.DefaultLifetime/2 + time.Minute)

	req := httptest.NewRequest(http.MethodGet, "/v8/management/account", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	req.Host = "cakebox.example.com:8443"
	req.Header.Set("Cookie", cookieHeader)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	setCookie := rec.Header().Get("Set-Cookie")
	if setCookie == "" {
		t.Fatal("expected a refreshed Set-Cookie")
	}
	if !strings.Contains(setCookie, "Secure") {
		t.Fatalf("Set-Cookie = %q, expected Secure because Host matches an https passkey origin", setCookie)
	}
}

func TestPostSessionLoginWrongPassword(t *testing.T) {
	h := newAccountHandler(t, mgmtauth.SystemClock{})
	engine := newTestEngine(h)

	rec := doRequest(engine, http.MethodPost, "/v8/management/session/login", loginJSON("admin", "totally-wrong"), nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", rec.Code, rec.Body.String())
	}
}

func TestPostSessionLoginNoAccount(t *testing.T) {
	h := newTestHandlerBase(t, mgmtauth.SystemClock{})
	engine := newTestEngine(h)

	rec := doRequest(engine, http.MethodPost, "/v8/management/session/login", loginJSON("admin", "whatever12345"), nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
}

func TestPostSessionLoginThrottled(t *testing.T) {
	clock := newMockClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	h := newAccountHandler(t, clock)
	engine := newTestEngine(h)

	rec := doRequest(engine, http.MethodPost, "/v8/management/session/login", loginJSON("admin", "wrong"), nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("first attempt status = %d, want 401", rec.Code)
	}

	rec = doRequest(engine, http.MethodPost, "/v8/management/session/login", loginJSON("admin", testAccountPassword), nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second attempt status = %d, want 429; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		RetryAfter int `json:"retry_after"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode 429 body: %v", err)
	}
	if body.RetryAfter != 1 {
		t.Fatalf("retry_after = %d, want 1", body.RetryAfter)
	}

	clock.Advance(time.Second)
	rec = doRequest(engine, http.MethodPost, "/v8/management/session/login", loginJSON("admin", testAccountPassword), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("third attempt status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// TestPostSessionLoginConcurrentAttemptsSingleFlight drives 16 concurrent
// login requests at a real HTTP handler (argon2 and all) and asserts at
// most one of them was actually evaluated; the rest must observe 429.
func TestPostSessionLoginConcurrentAttemptsSingleFlight(t *testing.T) {
	h := newAccountHandler(t, mgmtauth.SystemClock{})
	engine := newTestEngine(h)

	const n = 16
	var wg sync.WaitGroup
	var evaluated atomic.Int32 // 200 or 401: the throttle let the attempt through
	var throttled atomic.Int32 // 429: blocked before verification
	start := make(chan struct{})

	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			<-start
			rec := doRequest(engine, http.MethodPost, "/v8/management/session/login", loginJSON("admin", "wrong-password"), nil)
			switch rec.Code {
			case http.StatusTooManyRequests:
				throttled.Add(1)
			case http.StatusUnauthorized, http.StatusOK:
				evaluated.Add(1)
			default:
				t.Errorf("unexpected status %d; body=%s", rec.Code, rec.Body.String())
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := evaluated.Load(); got != 1 {
		t.Fatalf("evaluated = %d concurrent login attempts, want exactly 1", got)
	}
	if got := throttled.Load(); got != n-1 {
		t.Fatalf("throttled = %d, want %d", got, n-1)
	}
}

func TestCSRFRejectsCrossSiteCookiePost(t *testing.T) {
	h := newAccountHandler(t, mgmtauth.SystemClock{})
	engine := newTestEngine(h)

	loginRec := doRequest(engine, http.MethodPost, "/v8/management/session/login", loginJSON("admin", testAccountPassword), nil)
	cookieHeader, _ := sessionCookieFrom(loginRec)

	rec := doRequest(engine, http.MethodPost, "/v8/management/account/sign-out-all", "", map[string]string{
		"Cookie": cookieHeader,
		"Origin": "https://evil.example.com",
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

// TestCSRFSecFetchSiteBranches exercises every Sec-Fetch-Site branch,
// including the "same-site" value (treated as a mismatch, even for GET) and
// the absent case, for both safe and unsafe methods.
func TestCSRFSecFetchSiteBranches(t *testing.T) {
	h := newAccountHandler(t, mgmtauth.SystemClock{})
	engine := newTestEngine(h)

	loginRec := doRequest(engine, http.MethodPost, "/v8/management/session/login", loginJSON("admin", testAccountPassword), nil)
	cookieHeader, _ := sessionCookieFrom(loginRec)

	cases := []struct {
		name       string
		method     string
		secFetch   string
		origin     string
		wantStatus int
	}{
		{"same-origin GET", http.MethodGet, "same-origin", "", http.StatusOK},
		{"none GET", http.MethodGet, "none", "", http.StatusOK},
		{"same-site GET rejected", http.MethodGet, "same-site", "", http.StatusForbidden},
		{"cross-site GET rejected", http.MethodGet, "cross-site", "", http.StatusForbidden},
		{"same-origin POST", http.MethodPost, "same-origin", "", http.StatusNoContent},
		{"same-site POST rejected", http.MethodPost, "same-site", "", http.StatusForbidden},
		{"cross-site POST rejected", http.MethodPost, "cross-site", "", http.StatusForbidden},
		{"absent GET allowed", http.MethodGet, "", "", http.StatusOK},
		{"absent POST with no origin rejected", http.MethodPost, "", "", http.StatusForbidden},
		{"absent POST with origin null rejected", http.MethodPost, "", "null", http.StatusForbidden},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := "/v8/management/account"
			if tc.method == http.MethodPost {
				path = "/v8/management/account/sign-out-all"
			}
			headers := map[string]string{"Cookie": cookieHeader}
			if tc.secFetch != "" {
				headers["Sec-Fetch-Site"] = tc.secFetch
			}
			if tc.origin != "" {
				headers["Origin"] = tc.origin
			}
			rec := doRequest(engine, tc.method, path, "", headers)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			// sign-out-all mutates the account (rotates session-secret), so
			// re-login for the next sub-test if it actually succeeded.
			if tc.method == http.MethodPost && rec.Code == http.StatusNoContent {
				loginRec = doRequest(engine, http.MethodPost, "/v8/management/session/login", loginJSON("admin", testAccountPassword), nil)
				cookieHeader, _ = sessionCookieFrom(loginRec)
			}
		})
	}
}

func TestCSRFAllowsSameOriginCookiePost(t *testing.T) {
	h := newAccountHandler(t, mgmtauth.SystemClock{})
	engine := newTestEngine(h)

	loginRec := doRequest(engine, http.MethodPost, "/v8/management/session/login", loginJSON("admin", testAccountPassword), nil)
	cookieHeader, _ := sessionCookieFrom(loginRec)

	rec := doRequest(engine, http.MethodGet, "/v8/management/account", "", map[string]string{
		"Cookie":         cookieHeader,
		"Sec-Fetch-Site": "same-origin",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	rec = doRequest(engine, http.MethodPost, "/v8/management/account/sign-out-all", "", map[string]string{
		"Cookie":         cookieHeader,
		"Sec-Fetch-Site": "same-origin",
	})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("POST status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
}

func TestBearerTokenWorksCrossOrigin(t *testing.T) {
	h := newAccountHandler(t, mgmtauth.SystemClock{})
	engine := newTestEngine(h)

	loginRec := doRequest(engine, http.MethodPost, "/v8/management/session/login", loginJSON("admin", testAccountPassword), nil)
	var resp struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(loginRec.Body.Bytes(), &resp)

	rec := doRequest(engine, http.MethodGet, "/v8/management/account", "", map[string]string{
		"Authorization": "Bearer " + resp.Token,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	rec = doRequest(engine, http.MethodPost, "/v8/management/account/sign-out-all", "", map[string]string{
		"Authorization": "Bearer " + resp.Token,
		"Origin":        "https://evil.example.com",
	})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("POST status = %d, want 204 (bearer bypasses CSRF); body=%s", rec.Code, rec.Body.String())
	}
}

// TestStaleCookieDoesNotFallThroughOrBanTheIP covers: a presented cpas_
// cookie that fails verification is rejected outright (401 "session
// expired"), never falls through to key auth, and never counts against the
// key's IP-ban bookkeeping -- five stale attempts must not block a
// subsequent correct key.
func TestStaleCookieDoesNotFallThroughOrBanTheIP(t *testing.T) {
	h := newAccountHandler(t, mgmtauth.SystemClock{})
	h.envSecret = "test-secret"
	engine := newTestEngine(h)

	staleCookie := SessionCookieName + "=" + mgmtauth.TokenPrefix + "not-a-real-token"
	for i := 0; i < 5; i++ {
		rec := doRequest(engine, http.MethodGet, "/v8/management/account", "", map[string]string{
			"Cookie":         staleCookie,
			"Sec-Fetch-Site": "same-origin",
		})
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status = %d, want 401", i, rec.Code)
		}
		var body struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if body.Error != "session expired" {
			t.Fatalf("attempt %d: error = %q, want \"session expired\"", i, body.Error)
		}
	}

	// The key must still work: a stale session credential must never have
	// counted against its failure bookkeeping.
	rec := doRequest(engine, http.MethodGet, "/v8/management/account", "", map[string]string{"X-Management-Key": "test-secret"})
	if rec.Code != http.StatusOK {
		t.Fatalf("key auth after stale cookies: status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// TestInvalidSessionBearerAlsoDoesNotFallThrough mirrors
// TestStaleCookieDoesNotFallThroughOrBanTheIP for the bearer path.
func TestInvalidSessionBearerAlsoDoesNotFallThrough(t *testing.T) {
	h := newAccountHandler(t, mgmtauth.SystemClock{})
	engine := newTestEngine(h)

	rec := doRequest(engine, http.MethodGet, "/v8/management/account", "", map[string]string{
		"Authorization": "Bearer " + mgmtauth.TokenPrefix + "garbage",
	})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", rec.Code, rec.Body.String())
	}
}

func TestKeyAuthStillReachesV0AndV8Routes(t *testing.T) {
	h := newTestHandlerBase(t, mgmtauth.SystemClock{})
	h.envSecret = "test-secret"
	engine := newTestEngine(h)

	rec := doRequest(engine, http.MethodGet, "/v0/management/config", "", map[string]string{"X-Management-Key": "test-secret"})
	if rec.Code != http.StatusOK {
		t.Fatalf("/v0 status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	rec = doRequest(engine, http.MethodGet, "/v8/management/probe", "", map[string]string{"X-Management-Key": "test-secret"})
	if rec.Code != http.StatusOK {
		t.Fatalf("/v8 status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	rec = doRequest(engine, http.MethodGet, "/v0/management/config", "", map[string]string{"X-Management-Key": "wrong"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong key status = %d, want 401", rec.Code)
	}
}

// TestSessionReachesV0Route proves a session bypasses allow-remote for a
// genuinely remote (non-loopback) client, not merely a loopback request
// that would pass other checks for unrelated reasons.
func TestSessionReachesV0Route(t *testing.T) {
	h := newAccountHandler(t, mgmtauth.SystemClock{})
	engine := newTestEngine(h)

	loginRec := doRequest(engine, http.MethodPost, "/v8/management/session/login", loginJSON("admin", testAccountPassword), nil)
	cookieHeader, _ := sessionCookieFrom(loginRec)

	const remoteAddr = "203.0.113.5:54321"

	// Sanity check: without a session, this remote, key-less client is
	// rejected (no allow-remote override, no key configured).
	sanity := doRequestFrom(engine, http.MethodGet, "/v0/management/config", "", nil, remoteAddr)
	if sanity.Code == http.StatusOK {
		t.Fatal("expected a remote request with no credential to be rejected")
	}

	rec := doRequestFrom(engine, http.MethodGet, "/v0/management/config", "", map[string]string{
		"Cookie":         cookieHeader,
		"Sec-Fetch-Site": "same-origin",
	}, remoteAddr)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// TestSessionLoginRemoteGate verifies /session/login and
// /session/passkey/finish honor the same local-or-allow-remote predicate as
// key auth, for a non-loopback client with no override.
func TestSessionLoginRemoteGate(t *testing.T) {
	h := newAccountHandler(t, mgmtauth.SystemClock{})
	engine := newTestEngine(h)

	rec := doRequestFrom(engine, http.MethodPost, "/v8/management/session/login", loginJSON("admin", testAccountPassword), nil, "203.0.113.5:1")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}

	// A local client is unaffected.
	rec = doRequest(engine, http.MethodPost, "/v8/management/session/login", loginJSON("admin", testAccountPassword), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("local status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func TestSessionPasskeyFinishRemoteGate(t *testing.T) {
	h := newAccountHandler(t, mgmtauth.SystemClock{})
	engine := newTestEngine(h)

	body, _ := json.Marshal(map[string]interface{}{"ceremony_id": "whatever", "credential": json.RawMessage(`{}`)})
	rec := doRequestFrom(engine, http.MethodPost, "/v8/management/session/passkey/finish", string(body), nil, "203.0.113.5:1")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

func TestGetSessionStatus(t *testing.T) {
	h := newAccountHandler(t, mgmtauth.SystemClock{})
	engine := newTestEngine(h)

	rec := doRequest(engine, http.MethodGet, "/v8/management/session/status", "", nil)
	var status struct {
		Account       bool   `json:"account"`
		Authenticated bool   `json:"authenticated"`
		Method        string `json:"method"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if !status.Account || status.Authenticated || status.Method != "" {
		t.Fatalf("status = %+v, want account=true authenticated=false method=\"\"", status)
	}

	loginRec := doRequest(engine, http.MethodPost, "/v8/management/session/login", loginJSON("admin", testAccountPassword), nil)
	cookieHeader, _ := sessionCookieFrom(loginRec)

	rec = doRequest(engine, http.MethodGet, "/v8/management/session/status", "", map[string]string{"Cookie": cookieHeader})
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if !status.Authenticated || status.Method != "password" {
		t.Fatalf("status = %+v, want authenticated=true method=password", status)
	}
}

// TestGetSessionStatusEffectivePasskeyOrigins verifies passkey_origins
// reflects the effective WebAuthn origin list (defaulting to
// ["https://<rp-id>"] when unset), not the raw stored config value.
func TestGetSessionStatusEffectivePasskeyOrigins(t *testing.T) {
	h := newAccountHandler(t, mgmtauth.SystemClock{})
	if _, err := h.loginStore.Mutate(func(acct *mgmtauth.Account) (*mgmtauth.Account, error) {
		acct.PasskeyRPID = "mgmt.example.com"
		return acct, nil
	}); err != nil {
		t.Fatalf("seed rp-id: %v", err)
	}
	engine := newTestEngine(h)

	var status struct {
		PasskeyRPID    string   `json:"passkey_rp_id"`
		PasskeyOrigins []string `json:"passkey_origins"`
	}

	rec := doRequest(engine, http.MethodGet, "/v8/management/session/status", "", nil)
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if status.PasskeyRPID != "mgmt.example.com" {
		t.Fatalf("passkey_rp_id = %q, want mgmt.example.com", status.PasskeyRPID)
	}
	if len(status.PasskeyOrigins) != 1 || status.PasskeyOrigins[0] != "https://mgmt.example.com" {
		t.Fatalf("passkey_origins = %v, want [https://mgmt.example.com] (defaulted)", status.PasskeyOrigins)
	}

	if _, err := h.loginStore.Mutate(func(acct *mgmtauth.Account) (*mgmtauth.Account, error) {
		acct.PasskeyOrigins = []string{"https://mgmt.example.com:8443"}
		return acct, nil
	}); err != nil {
		t.Fatalf("seed origins: %v", err)
	}

	rec = doRequest(engine, http.MethodGet, "/v8/management/session/status", "", nil)
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if len(status.PasskeyOrigins) != 1 || status.PasskeyOrigins[0] != "https://mgmt.example.com:8443" {
		t.Fatalf("passkey_origins = %v, want the configured origin list once set", status.PasskeyOrigins)
	}
}

func TestCeilSecondsAtLeastOne(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want int
	}{
		{0, 1},
		{500 * time.Millisecond, 1},
		{999 * time.Millisecond, 1},
		{time.Second, 1},
		{1500 * time.Millisecond, 2},
		{2 * time.Second, 2},
		{30 * time.Second, 30},
	}
	for _, tc := range cases {
		if got := ceilSecondsAtLeastOne(tc.in); got != tc.want {
			t.Errorf("ceilSecondsAtLeastOne(%v) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// TestPostSessionLogoutClearsCookie checks both the cleared value and
// Max-Age=0 explicitly (an OR of the two would pass if only one held).
func TestPostSessionLogoutClearsCookie(t *testing.T) {
	h := newAccountHandler(t, mgmtauth.SystemClock{})
	engine := newTestEngine(h)

	rec := doRequest(engine, http.MethodPost, "/v8/management/session/logout", "", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	setCookie := rec.Header().Get("Set-Cookie")
	if !strings.Contains(setCookie, SessionCookieName+"=;") {
		t.Fatalf("Set-Cookie = %q, missing cleared value", setCookie)
	}
	if !strings.Contains(setCookie, "Max-Age=0") {
		t.Fatalf("Set-Cookie = %q, missing Max-Age=0", setCookie)
	}
}

// TestGetConfigV8ContainsNoLoginData locks in the core design change: the
// login account lives outside config.yaml entirely, so neither the JSON nor
// the YAML view of /v8/management/config can ever contain it.
func TestGetConfigV8ContainsNoLoginData(t *testing.T) {
	h := newAccountHandler(t, mgmtauth.SystemClock{})
	if _, err := h.loginStore.Mutate(func(acct *mgmtauth.Account) (*mgmtauth.Account, error) {
		acct.PasskeyRPID = "mgmt.example.com"
		acct.Passkeys = []mgmtauth.PasskeyRecord{{ID: "cred-1", PublicKey: "pub-1", RPID: "mgmt.example.com", Name: "Key"}}
		return acct, nil
	}); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	h.envSecret = "test-secret"
	engine := newTestEngine(h)

	forbidden := []string{testAccountPassword, h.loginStore.Get().PasswordHash, h.loginStore.Get().SessionSecret, h.loginStore.Get().UserHandle, "cred-1", "pub-1", "mgmt.example.com"}

	rec := doRequest(engine, http.MethodGet, "/v8/management/config", "", map[string]string{"X-Management-Key": "test-secret"})
	if rec.Code != http.StatusOK {
		t.Fatalf("GET config status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	for _, leak := range forbidden {
		if leak != "" && strings.Contains(rec.Body.String(), leak) {
			t.Fatalf("GET /v8/management/config leaked %q: %s", leak, rec.Body.String())
		}
	}

	rec = doRequest(engine, http.MethodGet, "/v8/management/config.yaml", "", map[string]string{"X-Management-Key": "test-secret"})
	if rec.Code != http.StatusOK {
		t.Fatalf("GET config.yaml status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	for _, leak := range forbidden {
		if leak != "" && strings.Contains(rec.Body.String(), leak) {
			t.Fatalf("GET /v8/management/config.yaml leaked %q: %s", leak, rec.Body.String())
		}
	}
}
