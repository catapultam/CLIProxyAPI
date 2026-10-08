package management

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/mgmtauth"
)

// hmacSHA256 reproduces mgmtauth's unexported signPayload so this test can
// hand-craft a legacy (pre-"remember me") token signature without reaching
// into mgmtauth's internals.
func hmacSHA256(secret []byte, payloadB64 string) []byte {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(payloadB64))
	return mac.Sum(nil)
}

// loginJSONRemember builds a /session/login body with an explicit "remember"
// field, mirroring loginJSON's shape for the username/password pair.
func loginJSONRemember(username, password string, remember bool) string {
	b, _ := json.Marshal(map[string]interface{}{"username": username, "password": password, "remember": remember})
	return string(b)
}

// sessionResponseMeta decodes the standard session-response body fields
// relevant to the remember-me tests.
type sessionResponseMeta struct {
	Token     string `json:"token"`
	ExpiresAt string `json:"expires_at"`
	Remember  *bool  `json:"remember"`
}

func decodeSessionResponse(t *testing.T, rec *httptest.ResponseRecorder) sessionResponseMeta {
	t.Helper()
	var resp sessionResponseMeta
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode session response: %v; body=%s", err, rec.Body.String())
	}
	return resp
}

// TestPostSessionLoginAbsentRememberDefaultsTrue covers requirement 1: an
// absent "remember" field must behave exactly like today -- a 30-day token
// and a cookie carrying a Max-Age.
func TestPostSessionLoginAbsentRememberDefaultsTrue(t *testing.T) {
	clock := newMockClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	h := newAccountHandler(t, clock)
	engine := newTestEngine(h)

	rec := doRequest(engine, http.MethodPost, "/v8/management/session/login", loginJSON("admin", testAccountPassword), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	resp := decodeSessionResponse(t, rec)
	if resp.Remember != nil && !*resp.Remember {
		t.Fatalf("remember = %v, want true (or omitted)", *resp.Remember)
	}
	expiresAt, err := time.Parse(time.RFC3339, resp.ExpiresAt)
	if err != nil {
		t.Fatalf("expires_at = %q is not RFC3339: %v", resp.ExpiresAt, err)
	}
	if got, want := expiresAt, clock.Now().Add(mgmtauth.DefaultLifetime); !got.Equal(want) {
		t.Fatalf("expires_at = %v, want %v", got, want)
	}

	setCookie := rec.Header().Get("Set-Cookie")
	if !strings.Contains(setCookie, "Max-Age=") {
		t.Fatalf("Set-Cookie = %q, expected a Max-Age for a remembered session", setCookie)
	}
	if strings.Contains(setCookie, "Max-Age=0") {
		t.Fatalf("Set-Cookie = %q, expected a positive Max-Age, not the clear-cookie sentinel", setCookie)
	}
}

// TestPostSessionLoginRememberFalse covers requirements 1, 3 and 4: an
// explicit remember=false must mint a 12h token and a cookie with no
// Max-Age/Expires attribute at all (a true browser-session cookie).
func TestPostSessionLoginRememberFalse(t *testing.T) {
	clock := newMockClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	h := newAccountHandler(t, clock)
	engine := newTestEngine(h)

	rec := doRequest(engine, http.MethodPost, "/v8/management/session/login", loginJSONRemember("admin", testAccountPassword, false), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	resp := decodeSessionResponse(t, rec)
	if resp.Remember == nil || *resp.Remember {
		t.Fatalf("remember = %v, want false", resp.Remember)
	}
	expiresAt, err := time.Parse(time.RFC3339, resp.ExpiresAt)
	if err != nil {
		t.Fatalf("expires_at = %q is not RFC3339: %v", resp.ExpiresAt, err)
	}
	if got, want := expiresAt, clock.Now().Add(mgmtauth.BrowserSessionLifetime); !got.Equal(want) {
		t.Fatalf("expires_at = %v, want %v (BrowserSessionLifetime)", got, want)
	}

	setCookie := rec.Header().Get("Set-Cookie")
	if setCookie == "" {
		t.Fatal("expected a Set-Cookie header")
	}
	if strings.Contains(setCookie, "Max-Age") || strings.Contains(setCookie, "Expires") {
		t.Fatalf("Set-Cookie = %q, expected no Max-Age/Expires for remember=false", setCookie)
	}
	// Still a real, usable session cookie otherwise.
	if !strings.Contains(setCookie, "HttpOnly") || !strings.Contains(setCookie, "SameSite=Strict") {
		t.Fatalf("Set-Cookie = %q, missing expected attributes", setCookie)
	}
}

// TestSlidingRefreshPreservesRememberFalse covers requirement 4's key trap:
// the sliding refresh inside tryAuthenticateSession must keep a
// remember=false session as a no-Max-Age browser-session cookie (not
// upgrade it to a persistent one), and must slide it using its OWN 12h
// lifetime (refresh trigger at <6h remaining, not <15 days).
func TestSlidingRefreshPreservesRememberFalse(t *testing.T) {
	clock := newMockClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	h := newAccountHandler(t, clock)
	engine := newTestEngine(h)

	loginRec := doRequest(engine, http.MethodPost, "/v8/management/session/login", loginJSONRemember("admin", testAccountPassword, false), nil)
	cookieHeader, _ := sessionCookieFrom(loginRec)
	if cookieHeader == "" {
		t.Fatal("expected a session cookie from login")
	}

	// Just past the halfway point of the 12h browser-session lifetime (6h):
	// well short of DefaultLifetime's halfway point, so this only proves a
	// refresh happened if the handler is using the 12h lifetime, not 30d.
	clock.Advance(mgmtauth.BrowserSessionLifetime/2 + time.Minute)

	rec := doRequest(engine, http.MethodGet, "/v8/management/account", "", map[string]string{
		"Cookie":         cookieHeader,
		"Sec-Fetch-Site": "same-origin",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	setCookie := rec.Header().Get("Set-Cookie")
	if setCookie == "" {
		t.Fatal("expected a refreshed Set-Cookie past the 6h halfway point")
	}
	if strings.Contains(setCookie, "Max-Age") || strings.Contains(setCookie, "Expires") {
		t.Fatalf("Set-Cookie = %q, refreshed remember=false cookie must still have no Max-Age/Expires", setCookie)
	}

	refreshedCookieHeader, refreshedToken := sessionCookieFrom(rec)
	if refreshedCookieHeader == "" {
		t.Fatal("expected the refreshed cookie to carry a cpas_ token")
	}
	account := h.loginStore.Get()
	secret, err := decodeLoginSecret(account.SessionSecret)
	if err != nil {
		t.Fatalf("decodeLoginSecret: %v", err)
	}
	claims, err := mgmtauth.VerifyToken(secret, refreshedToken, clock.Now())
	if err != nil {
		t.Fatalf("VerifyToken on refreshed token: %v", err)
	}
	if claims.Remember {
		t.Fatal("refreshed token must still carry remember=false")
	}
	if want := clock.Now().Add(mgmtauth.BrowserSessionLifetime); !claims.ExpiresAt.Equal(want) {
		t.Fatalf("refreshed ExpiresAt = %v, want %v (slid using the 12h lifetime)", claims.ExpiresAt, want)
	}
}

// TestSlidingRefreshRememberFalseBearer is TestSlidingRefreshPreservesRememberFalse's
// bearer-token counterpart: X-CPA-Session-Refresh must also carry a
// still-not-remembered token.
func TestSlidingRefreshRememberFalseBearer(t *testing.T) {
	clock := newMockClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	h := newAccountHandler(t, clock)
	engine := newTestEngine(h)

	loginRec := doRequest(engine, http.MethodPost, "/v8/management/session/login", loginJSONRemember("admin", testAccountPassword, false), nil)
	resp := decodeSessionResponse(t, loginRec)

	clock.Advance(mgmtauth.BrowserSessionLifetime/2 + time.Minute)

	rec := doRequest(engine, http.MethodGet, "/v8/management/account", "", map[string]string{
		"Authorization": "Bearer " + resp.Token,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	refreshed := rec.Header().Get("X-CPA-Session-Refresh")
	if refreshed == "" {
		t.Fatal("expected X-CPA-Session-Refresh past the 6h halfway point")
	}
	account := h.loginStore.Get()
	secret, err := decodeLoginSecret(account.SessionSecret)
	if err != nil {
		t.Fatalf("decodeLoginSecret: %v", err)
	}
	claims, err := mgmtauth.VerifyToken(secret, refreshed, clock.Now())
	if err != nil {
		t.Fatalf("VerifyToken on refreshed bearer token: %v", err)
	}
	if claims.Remember {
		t.Fatal("refreshed bearer token must still carry remember=false")
	}
}

// TestOldTokenWithoutRememberClaimRefreshesAsRemembered covers requirement
// 4's compatibility half: a token minted before "remember me" existed (no
// claim at all) must still verify, and when it slides forward it must be
// treated as remembered (30-day lifetime, Max-Age cookie) -- never as a
// browser session.
func TestOldTokenWithoutRememberClaimRefreshesAsRemembered(t *testing.T) {
	clock := newMockClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	h := newAccountHandler(t, clock)
	engine := newTestEngine(h)

	account := h.loginStore.Get()
	secret, err := decodeLoginSecret(account.SessionSecret)
	if err != nil {
		t.Fatalf("decodeLoginSecret: %v", err)
	}

	// Hand-craft a legacy token payload with no "eph" field, exactly what
	// IssueToken produced before this feature existed.
	legacyPayload := struct {
		IssuedAt  int64  `json:"iat"`
		ExpiresAt int64  `json:"exp"`
		Method    string `json:"m"`
	}{
		IssuedAt:  clock.Now().Unix(),
		ExpiresAt: clock.Now().Add(mgmtauth.DefaultLifetime).Unix(),
		Method:    string(mgmtauth.MethodPassword),
	}
	raw, err := json.Marshal(legacyPayload)
	if err != nil {
		t.Fatalf("marshal legacy payload: %v", err)
	}
	payloadB64 := base64.RawURLEncoding.EncodeToString(raw)
	// mgmtauth.signPayload is unexported and lives in a different package,
	// so reconstruct the HMAC inline rather than reach across packages.
	legacyToken := mgmtauth.TokenPrefix + payloadB64 + "." + base64.RawURLEncoding.EncodeToString(hmacSHA256(secret, payloadB64))

	cookieHeader := SessionCookieName + "=" + legacyToken
	// Sanity check: the hand-crafted token must actually verify before
	// relying on it to exercise the refresh path.
	if _, err := mgmtauth.VerifyToken(secret, legacyToken, clock.Now()); err != nil {
		t.Fatalf("hand-crafted legacy token failed to verify: %v", err)
	}

	clock.Advance(mgmtauth.DefaultLifetime/2 + time.Minute)

	rec := doRequest(engine, http.MethodGet, "/v8/management/account", "", map[string]string{
		"Cookie":         cookieHeader,
		"Sec-Fetch-Site": "same-origin",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	setCookie := rec.Header().Get("Set-Cookie")
	if setCookie == "" {
		t.Fatal("expected a refreshed Set-Cookie past the 15-day halfway point")
	}
	if !strings.Contains(setCookie, "Max-Age=") || strings.Contains(setCookie, "Max-Age=0") {
		t.Fatalf("Set-Cookie = %q, a legacy token must refresh as remembered (positive Max-Age)", setCookie)
	}

	_, refreshedToken := sessionCookieFrom(rec)
	claims, err := mgmtauth.VerifyToken(secret, refreshedToken, clock.Now())
	if err != nil {
		t.Fatalf("VerifyToken on refreshed token: %v", err)
	}
	if !claims.Remember {
		t.Fatal("refreshed legacy token must carry remember=true")
	}
	if want := clock.Now().Add(mgmtauth.DefaultLifetime); !claims.ExpiresAt.Equal(want) {
		t.Fatalf("refreshed ExpiresAt = %v, want %v (DefaultLifetime)", claims.ExpiresAt, want)
	}
}

// TestPostSessionPasskeyFinishHonorsRemember covers requirement 6's passkey
// half: /session/passkey/finish must accept and apply "remember" exactly
// like /session/login.
func TestPostSessionPasskeyFinishHonorsRemember(t *testing.T) {
	const rpID = "mgmt.example.com"
	const origin = "https://mgmt.example.com"

	clock := newMockClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	h := newAccountHandler(t, clock)
	if _, err := h.loginStore.Mutate(func(acct *mgmtauth.Account) (*mgmtauth.Account, error) {
		acct.PasskeyRPID = rpID
		acct.PasskeyOrigins = []string{origin}
		return acct, nil
	}); err != nil {
		t.Fatalf("seed passkey settings: %v", err)
	}
	engine := newTestEngine(h)

	// Register a passkey over a password session.
	loginRec := doRequest(engine, http.MethodPost, "/v8/management/session/login", loginJSON("admin", testAccountPassword), nil)
	cookieHeader, _ := sessionCookieFrom(loginRec)
	authedHeaders := map[string]string{"Cookie": cookieHeader, "Sec-Fetch-Site": "same-origin"}

	beginRec := doRequest(engine, http.MethodPost, "/v8/management/account/passkeys/begin", currentPasswordBody(testAccountPassword), authedHeaders)
	var beginResp struct {
		CeremonyID string `json:"ceremony_id"`
		Options    struct {
			PublicKey struct {
				Challenge string `json:"challenge"`
			} `json:"publicKey"`
		} `json:"options"`
	}
	if err := json.Unmarshal(beginRec.Body.Bytes(), &beginResp); err != nil {
		t.Fatalf("decode passkeys/begin: %v; body=%s", err, beginRec.Body.String())
	}

	authenticator := newSoftAuthenticator(t, []byte("remember-me-credential"))
	regJSON := authenticator.registrationResponseJSON(t, rpID, origin, decodeB64URLForTest(t, beginResp.Options.PublicKey.Challenge))
	finishBody, _ := json.Marshal(map[string]interface{}{
		"ceremony_id": beginResp.CeremonyID,
		"name":        "Test Key",
		"credential":  json.RawMessage(regJSON),
	})
	doRequest(engine, http.MethodPost, "/v8/management/account/passkeys/finish", string(finishBody), authedHeaders)

	// Now log in via the passkey with remember=false.
	loginBeginRec := doRequest(engine, http.MethodPost, "/v8/management/session/passkey/begin", "", nil)
	var loginBeginResp struct {
		CeremonyID string `json:"ceremony_id"`
		Options    struct {
			PublicKey struct {
				Challenge string `json:"challenge"`
			} `json:"publicKey"`
		} `json:"options"`
	}
	if err := json.Unmarshal(loginBeginRec.Body.Bytes(), &loginBeginResp); err != nil {
		t.Fatalf("decode passkey/begin: %v; body=%s", err, loginBeginRec.Body.String())
	}
	userHandle := decodeB64URLForTest(t, h.loginStore.Get().UserHandle)
	assertionJSON := authenticator.assertionResponseJSON(t, rpID, origin, decodeB64URLForTest(t, loginBeginResp.Options.PublicKey.Challenge), userHandle)
	loginFinishBody, _ := json.Marshal(map[string]interface{}{
		"ceremony_id": loginBeginResp.CeremonyID,
		"credential":  json.RawMessage(assertionJSON),
		"remember":    false,
	})
	passkeyLoginRec := doRequest(engine, http.MethodPost, "/v8/management/session/passkey/finish", string(loginFinishBody), nil)
	if passkeyLoginRec.Code != http.StatusOK {
		t.Fatalf("passkey/finish status = %d, want 200; body=%s", passkeyLoginRec.Code, passkeyLoginRec.Body.String())
	}
	resp := decodeSessionResponse(t, passkeyLoginRec)
	if resp.Remember == nil || *resp.Remember {
		t.Fatalf("remember = %v, want false", resp.Remember)
	}
	setCookie := passkeyLoginRec.Header().Get("Set-Cookie")
	if strings.Contains(setCookie, "Max-Age") || strings.Contains(setCookie, "Expires") {
		t.Fatalf("Set-Cookie = %q, expected no Max-Age/Expires for a passkey login with remember=false", setCookie)
	}
}
