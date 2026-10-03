package management

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/mgmtauth"
)

const staleBearer = "Bearer " + mgmtauth.TokenPrefix + "stale-token"

// TestSessionStatusWithStaleBearerNeverBansKey covers B1: GET /session/status
// must never feed a cpas_ value into key auth or do failure bookkeeping.
func TestSessionStatusWithStaleBearerNeverBansKey(t *testing.T) {
	h := newAccountHandler(t, mgmtauth.SystemClock{})
	h.envSecret = "test-secret"
	engine := newTestEngine(h)

	for i := 0; i < 6; i++ {
		rec := doRequest(engine, http.MethodGet, "/v8/management/session/status", "", map[string]string{"Authorization": staleBearer})
		if rec.Code != http.StatusOK {
			t.Fatalf("status #%d = %d, want 200", i, rec.Code)
		}
	}
	rec := doRequest(engine, http.MethodGet, "/v8/management/account", "", map[string]string{"X-Management-Key": "test-secret"})
	if rec.Code != http.StatusOK {
		t.Fatalf("key after stale status calls: status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// TestSessionStatusWrongKeyDoesNotCountFailures: even a genuinely wrong key
// guess on status is non-counting.
func TestSessionStatusWrongKeyDoesNotCountFailures(t *testing.T) {
	h := newAccountHandler(t, mgmtauth.SystemClock{})
	h.envSecret = "test-secret"
	engine := newTestEngine(h)
	for i := 0; i < 6; i++ {
		doRequest(engine, http.MethodGet, "/v8/management/session/status", "", map[string]string{"X-Management-Key": "wrong"})
	}
	rec := doRequest(engine, http.MethodGet, "/v8/management/account", "", map[string]string{"X-Management-Key": "test-secret"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

// TestStaleSessionPlusValidKeyFallsThroughAndClearsCookie covers S1 and B.
func TestStaleSessionPlusValidKeyFallsThroughAndClearsCookie(t *testing.T) {
	h := newAccountHandler(t, mgmtauth.SystemClock{})
	h.envSecret = "test-secret"
	engine := newTestEngine(h)

	rec := doRequest(engine, http.MethodGet, "/v8/management/account", "", map[string]string{
		"Cookie":           SessionCookieName + "=" + mgmtauth.TokenPrefix + "stale",
		"X-Management-Key": "test-secret",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("stale cookie + key: status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	setCookie := rec.Header().Get("Set-Cookie")
	if !strings.Contains(setCookie, "Max-Age=0") || !strings.Contains(setCookie, "Path=/v8/management") {
		t.Fatalf("Set-Cookie = %q, want a cleared cookie", setCookie)
	}

	rec = doRequest(engine, http.MethodGet, "/v8/management/account", "", map[string]string{
		"Authorization":    staleBearer,
		"X-Management-Key": "test-secret",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("stale bearer + key: status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// TestStaleCookieOnlyClearsCookie covers B for the middleware 401 path and
// for GET /session/status.
func TestStaleCookieOnlyClearsCookie(t *testing.T) {
	h := newAccountHandler(t, mgmtauth.SystemClock{})
	engine := newTestEngine(h)
	stale := SessionCookieName + "=" + mgmtauth.TokenPrefix + "stale"

	rec := doRequest(engine, http.MethodGet, "/v8/management/account", "", map[string]string{"Cookie": stale, "Sec-Fetch-Site": "same-origin"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if sc := rec.Header().Get("Set-Cookie"); !strings.Contains(sc, "Max-Age=0") || !strings.Contains(sc, "Path=/v8/management") {
		t.Fatalf("middleware Set-Cookie = %q, want cleared cookie", sc)
	}

	rec = doRequest(engine, http.MethodGet, "/v8/management/session/status", "", map[string]string{"Cookie": stale})
	if sc := rec.Header().Get("Set-Cookie"); !strings.Contains(sc, "Max-Age=0") || !strings.Contains(sc, "Path=/v8/management") {
		t.Fatalf("status Set-Cookie = %q, want cleared cookie", sc)
	}
}

// TestBearerWinsOverCSRFBlockedCookie covers N5.
func TestBearerWinsOverCSRFBlockedCookie(t *testing.T) {
	h := newAccountHandler(t, mgmtauth.SystemClock{})
	engine := newTestEngine(h)
	loginRec := doRequest(engine, http.MethodPost, "/v8/management/session/login", loginJSON("admin", testAccountPassword), nil)
	cookie, token := sessionCookieFrom(loginRec)

	rec := doRequest(engine, http.MethodPost, "/v8/management/account/sign-out-all", "", map[string]string{
		"Cookie":         cookie,
		"Authorization":  "Bearer " + token,
		"Sec-Fetch-Site": "cross-site",
	})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (valid bearer must win); body=%s", rec.Code, rec.Body.String())
	}
}

// TestSessionRequiresAccount covers N3.
func TestSessionRequiresAccount(t *testing.T) {
	h := newTestHandlerBase(t, mgmtauth.SystemClock{})
	engine := newTestEngine(h)
	tok, _, err := mgmtauth.IssueToken([]byte("some-secret"), mgmtauth.MethodPassword, time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	rec := doRequest(engine, http.MethodGet, "/v8/management/session/status", "", map[string]string{"Authorization": "Bearer " + tok})
	var st struct {
		Authenticated bool `json:"authenticated"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &st)
	if st.Authenticated {
		t.Fatal("status must not report authenticated without an account")
	}
}

// TestPublicBodyIsCapped covers N9.
func TestPublicBodyIsCapped(t *testing.T) {
	h := newAccountHandler(t, mgmtauth.SystemClock{})
	engine := newTestEngine(h)
	big := `{"username":"admin","password":"` + strings.Repeat("a", 128*1024) + `"}`
	rec := doRequest(engine, http.MethodPost, "/v8/management/session/login", big, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an oversized body", rec.Code)
	}
}

// TestPasskeyBeginRemoteGate covers S3's remote predicate.
func TestPasskeyBeginRemoteGate(t *testing.T) {
	h := newAccountHandler(t, mgmtauth.SystemClock{})
	engine := newTestEngine(h)
	rec := doRequestFrom(engine, http.MethodPost, "/v8/management/session/passkey/begin", "", nil, "203.0.113.5:1")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

// TestLoginTrimsUsername covers N11.
func TestLoginTrimsUsername(t *testing.T) {
	h := newAccountHandler(t, mgmtauth.SystemClock{})
	engine := newTestEngine(h)
	rec := doRequest(engine, http.MethodPost, "/v8/management/session/login", loginJSON("  admin  ", testAccountPassword), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

// TestBrokenStoreReturns503 covers S6 at the handler layer.
func TestBrokenStoreReturns503(t *testing.T) {
	h := newTestHandlerBase(t, mgmtauth.SystemClock{})
	h.envSecret = "test-secret"
	h.allowRemoteOverride = true
	path := mgmtauth.ResolveStorePath(h.configFilePath, "")
	if err := os.WriteFile(path, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.loginStore = mgmtauth.NewStore(h.configFilePath, "")
	h.loginStore.Load()
	engine := newTestEngine(h)

	rec := doRequest(engine, http.MethodPut, "/v8/management/account", putAccountBody("admin", "a-strong-password", ""), map[string]string{"X-Management-Key": "test-secret"})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", rec.Code, rec.Body.String())
	}
	if b, _ := os.ReadFile(path); string(b) != "garbage" {
		t.Fatal("broken sidecar must not be overwritten")
	}
	rec = doRequest(engine, http.MethodGet, "/v0/management/config", "", map[string]string{"X-Management-Key": "test-secret"})
	if rec.Code != http.StatusOK {
		t.Fatalf("key route status = %d, want 200", rec.Code)
	}
}
