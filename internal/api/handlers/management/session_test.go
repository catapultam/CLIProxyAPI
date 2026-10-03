package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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

	return engine
}

const testAccountPassword = "correct-horse-battery-staple"

// newAccountHandler builds a Handler with a pre-configured username/password
// account (no passkeys), driven by clock for deterministic tests.
func newAccountHandler(t *testing.T, clock mgmtauth.Clock) *Handler {
	t.Helper()
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
	cfg := &config.Config{}
	cfg.RemoteManagement.Login = config.LoginConfig{
		Username:      "admin",
		PasswordHash:  hash,
		SessionSecret: secret,
		UserHandle:    handle,
	}
	return &Handler{
		cfg:               cfg,
		configFilePath:    writeTestConfigFile(t),
		failedAttempts:    make(map[string]*attemptInfo),
		loginThrottle:     mgmtauth.NewThrottle(clock),
		passkeyCeremonies: mgmtauth.NewCeremonyCache(clock),
		clock:             clock,
	}
}

func doRequest(engine *gin.Engine, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	var reader *strings.Reader
	if body != "" {
		reader = strings.NewReader(body)
	} else {
		reader = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, reader)
	req.RemoteAddr = "127.0.0.1:12345"
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
	if !strings.Contains(setCookie, "HttpOnly") || !strings.Contains(setCookie, "SameSite=Strict") || !strings.Contains(setCookie, "Path=/") {
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
		"Origin": "https://cakebox.wyrm-cat.ts.net",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	setCookie := rec.Header().Get("Set-Cookie")
	if !strings.Contains(setCookie, "Secure") {
		t.Fatalf("Set-Cookie = %q, expected Secure when Origin is https", setCookie)
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
	h := &Handler{
		cfg:               &config.Config{},
		configFilePath:    writeTestConfigFile(t),
		failedAttempts:    make(map[string]*attemptInfo),
		loginThrottle:     mgmtauth.NewThrottle(mgmtauth.SystemClock{}),
		passkeyCeremonies: mgmtauth.NewCeremonyCache(mgmtauth.SystemClock{}),
		clock:             mgmtauth.SystemClock{},
	}
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

	// First attempt fails and opens a 1s backoff window.
	rec := doRequest(engine, http.MethodPost, "/v8/management/session/login", loginJSON("admin", "wrong"), nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("first attempt status = %d, want 401", rec.Code)
	}

	// A second attempt inside the window is throttled, even with the right password.
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

	// Advancing past the window allows the next (correct) attempt through.
	clock.Advance(time.Second)
	rec = doRequest(engine, http.MethodPost, "/v8/management/session/login", loginJSON("admin", testAccountPassword), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("third attempt status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func TestCSRFRejectsCrossSiteCookiePost(t *testing.T) {
	h := newAccountHandler(t, mgmtauth.SystemClock{})
	engine := newTestEngine(h)

	loginRec := doRequest(engine, http.MethodPost, "/v8/management/session/login", loginJSON("admin", testAccountPassword), nil)
	if loginRec.Code != http.StatusOK {
		t.Fatalf("login status = %d, want 200", loginRec.Code)
	}
	cookieHeader, _ := sessionCookieFrom(loginRec)

	// A cross-site POST that carries the cookie but neither a same-origin
	// Sec-Fetch-Site nor a matching/allowed Origin must be rejected.
	rec := doRequest(engine, http.MethodPost, "/v8/management/account/sign-out-all", "", map[string]string{
		"Cookie": cookieHeader,
		"Origin": "https://evil.example.com",
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
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

	// Bearer auth is not cookie-driven, so an unsafe method needs no CSRF headers.
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

func TestKeyAuthStillReachesV0AndV8Routes(t *testing.T) {
	h := &Handler{
		cfg:               &config.Config{},
		configFilePath:    writeTestConfigFile(t),
		failedAttempts:    make(map[string]*attemptInfo),
		envSecret:         "test-secret",
		loginThrottle:     mgmtauth.NewThrottle(mgmtauth.SystemClock{}),
		passkeyCeremonies: mgmtauth.NewCeremonyCache(mgmtauth.SystemClock{}),
		clock:             mgmtauth.SystemClock{},
	}
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

func TestSessionReachesV0Route(t *testing.T) {
	h := newAccountHandler(t, mgmtauth.SystemClock{})
	engine := newTestEngine(h)

	loginRec := doRequest(engine, http.MethodPost, "/v8/management/session/login", loginJSON("admin", testAccountPassword), nil)
	cookieHeader, _ := sessionCookieFrom(loginRec)

	// The session bypasses allow-remote entirely: no key, no local client, no
	// override configured, yet the request must still succeed.
	rec := doRequest(engine, http.MethodGet, "/v0/management/config", "", map[string]string{
		"Cookie":         cookieHeader,
		"Sec-Fetch-Site": "same-origin",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func TestGetSessionStatus(t *testing.T) {
	h := newAccountHandler(t, mgmtauth.SystemClock{})
	engine := newTestEngine(h)

	// No credential at all: authenticated=false, and no failure recorded.
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
	h.mu.Lock()
	h.cfg.RemoteManagement.Login.PasskeyRPID = "mgmt.example.com"
	h.mu.Unlock()
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

	h.mu.Lock()
	h.cfg.RemoteManagement.Login.PasskeyOrigins = []string{"https://mgmt.example.com:8443"}
	h.mu.Unlock()

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

func TestPostSessionLogoutClearsCookie(t *testing.T) {
	h := newAccountHandler(t, mgmtauth.SystemClock{})
	engine := newTestEngine(h)

	rec := doRequest(engine, http.MethodPost, "/v8/management/session/logout", "", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	setCookie := rec.Header().Get("Set-Cookie")
	if !strings.Contains(setCookie, SessionCookieName+"=;") && !strings.Contains(setCookie, "Max-Age=0") {
		t.Fatalf("Set-Cookie = %q, expected it to clear the cookie", setCookie)
	}
}
