package management

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/mgmtauth"
)

// keyOnlyHandler builds a Handler with a management key but no login
// account yet, for first-setup PUT /account tests.
func keyOnlyHandler(t *testing.T) *Handler {
	t.Helper()
	return &Handler{
		cfg:               &config.Config{},
		configFilePath:    writeTestConfigFile(t),
		failedAttempts:    make(map[string]*attemptInfo),
		envSecret:         "test-secret",
		loginThrottle:     mgmtauth.NewThrottle(mgmtauth.SystemClock{}),
		passkeyCeremonies: mgmtauth.NewCeremonyCache(mgmtauth.SystemClock{}),
		clock:             mgmtauth.SystemClock{},
	}
}

func putAccountBody(username, password, currentPassword string) string {
	b, _ := json.Marshal(map[string]string{
		"username":         username,
		"password":         password,
		"current_password": currentPassword,
	})
	return string(b)
}

func TestPutAccountFirstSetupOverKeyNeedsNoCurrentPassword(t *testing.T) {
	h := keyOnlyHandler(t)
	engine := newTestEngine(h)

	rec := doRequest(engine, http.MethodPut, "/v8/management/account", putAccountBody("admin", "a-strong-password", ""), map[string]string{
		"X-Management-Key": "test-secret",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	h.mu.Lock()
	login := h.cfg.RemoteManagement.Login
	h.mu.Unlock()
	if login.Username != "admin" || login.PasswordHash == "" {
		t.Fatalf("account not persisted in memory: %+v", login)
	}
	if login.SessionSecret == "" || login.UserHandle == "" {
		t.Fatal("expected session-secret and user-handle to be generated on first setup")
	}

	var resp struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if !strings.HasPrefix(resp.Token, mgmtauth.TokenPrefix) {
		t.Fatalf("token = %q, want cpas_ prefix", resp.Token)
	}
}

func TestPutAccountRejectsWeakPassword(t *testing.T) {
	h := keyOnlyHandler(t)
	engine := newTestEngine(h)

	rec := doRequest(engine, http.MethodPut, "/v8/management/account", putAccountBody("admin", "short", ""), map[string]string{
		"X-Management-Key": "test-secret",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestPutAccountFirstSetupRequiresPassword(t *testing.T) {
	h := keyOnlyHandler(t)
	engine := newTestEngine(h)

	rec := doRequest(engine, http.MethodPut, "/v8/management/account", putAccountBody("admin", "", ""), map[string]string{
		"X-Management-Key": "test-secret",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// TestPutAccountUsernameOnlyChangeKeepsPassword covers the "empty password
// means keep the current one" rule: a username-only update over the
// management key must succeed without requiring current_password, and the
// original password must still verify afterward.
func TestPutAccountUsernameOnlyChangeKeepsPassword(t *testing.T) {
	h := newAccountHandler(t, mgmtauth.SystemClock{})
	h.envSecret = "test-secret"
	engine := newTestEngine(h)

	rec := doRequest(engine, http.MethodPut, "/v8/management/account", putAccountBody("renamed-admin", "", ""), map[string]string{
		"X-Management-Key": "test-secret",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	login := h.currentLoginConfig()
	if login.Username != "renamed-admin" {
		t.Fatalf("username = %q, want renamed-admin", login.Username)
	}
	ok, err := mgmtauth.VerifyPassword(login.PasswordHash, testAccountPassword)
	if err != nil || !ok {
		t.Fatalf("expected the original password to still verify, ok=%v err=%v", ok, err)
	}
}

// TestPutAccountUsernameOnlyChangeOverSessionRequiresCurrentPassword covers
// the updated rule that current_password is required for ANY change over a
// session, not just a password change.
func TestPutAccountUsernameOnlyChangeOverSessionRequiresCurrentPassword(t *testing.T) {
	h := newAccountHandler(t, mgmtauth.SystemClock{})
	engine := newTestEngine(h)

	loginRec := doRequest(engine, http.MethodPost, "/v8/management/session/login", loginJSON("admin", testAccountPassword), nil)
	cookieHeader, _ := sessionCookieFrom(loginRec)
	headers := map[string]string{"Cookie": cookieHeader, "Sec-Fetch-Site": "same-origin"}

	rec := doRequest(engine, http.MethodPut, "/v8/management/account", putAccountBody("renamed-admin", "", ""), headers)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}

	rec = doRequest(engine, http.MethodPut, "/v8/management/account", putAccountBody("renamed-admin", "", testAccountPassword), headers)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if h.currentLoginConfig().Username != "renamed-admin" {
		t.Fatalf("username = %q, want renamed-admin", h.currentLoginConfig().Username)
	}
}

// TestPutAccountUsernameOnlyChangeDoesNotRotateSession verifies session-secret
// is rotated only when the password actually changes: a username-only change
// must leave a token issued before the change still valid afterward.
func TestPutAccountUsernameOnlyChangeDoesNotRotateSession(t *testing.T) {
	h := newAccountHandler(t, mgmtauth.SystemClock{})
	engine := newTestEngine(h)

	loginRec := doRequest(engine, http.MethodPost, "/v8/management/session/login", loginJSON("admin", testAccountPassword), nil)
	cookieHeader, _ := sessionCookieFrom(loginRec)
	headers := map[string]string{"Cookie": cookieHeader, "Sec-Fetch-Site": "same-origin"}

	rec := doRequest(engine, http.MethodPut, "/v8/management/account", putAccountBody("renamed-admin", "", testAccountPassword), headers)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// The token issued before the username-only change must still work.
	rec = doRequest(engine, http.MethodGet, "/v8/management/account", "", headers)
	if rec.Code != http.StatusOK {
		t.Fatalf("pre-change token status = %d, want 200 (session-secret should not rotate); body=%s", rec.Code, rec.Body.String())
	}
}

func TestPutAccountOverSessionRequiresCurrentPassword(t *testing.T) {
	h := newAccountHandler(t, mgmtauth.SystemClock{})
	engine := newTestEngine(h)

	loginRec := doRequest(engine, http.MethodPost, "/v8/management/session/login", loginJSON("admin", testAccountPassword), nil)
	cookieHeader, _ := sessionCookieFrom(loginRec)
	headers := map[string]string{"Cookie": cookieHeader, "Sec-Fetch-Site": "same-origin"}

	// Missing current_password.
	rec := doRequest(engine, http.MethodPut, "/v8/management/account", putAccountBody("admin", "a-new-strong-password", ""), headers)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("missing current_password status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}

	// Wrong current_password.
	rec = doRequest(engine, http.MethodPut, "/v8/management/account", putAccountBody("admin", "a-new-strong-password", "nope-not-it"), headers)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("wrong current_password status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

func TestPutAccountOverKeyDoesNotRequireCurrentPassword(t *testing.T) {
	h := newAccountHandler(t, mgmtauth.SystemClock{})
	h.envSecret = "test-secret"
	engine := newTestEngine(h)

	rec := doRequest(engine, http.MethodPut, "/v8/management/account", putAccountBody("admin", "a-new-strong-password", ""), map[string]string{
		"X-Management-Key": "test-secret",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// TestPasswordChangeRotatesSessionAndOldTokenFails covers: a password change
// over a valid session returns a fresh session that works, while the old
// token (issued before the change) is rejected afterward.
func TestPasswordChangeRotatesSessionAndOldTokenFails(t *testing.T) {
	h := newAccountHandler(t, mgmtauth.SystemClock{})
	engine := newTestEngine(h)

	loginRec := doRequest(engine, http.MethodPost, "/v8/management/session/login", loginJSON("admin", testAccountPassword), nil)
	oldCookieHeader, _ := sessionCookieFrom(loginRec)

	rec := doRequest(engine, http.MethodPut, "/v8/management/account", putAccountBody("admin", "a-new-strong-password", testAccountPassword), map[string]string{
		"Cookie":         oldCookieHeader,
		"Sec-Fetch-Site": "same-origin",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("password change status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	newCookieHeader := SessionCookieName + "=" + resp.Token

	// The old token must now fail: no management key is configured here, so
	// Middleware() falls through to the key check, which reports "key not set".
	rec = doRequest(engine, http.MethodGet, "/v8/management/account", "", map[string]string{
		"Cookie":         oldCookieHeader,
		"Sec-Fetch-Site": "same-origin",
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("old token status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}

	// The fresh token from the password-change response works.
	rec = doRequest(engine, http.MethodGet, "/v8/management/account", "", map[string]string{
		"Cookie":         newCookieHeader,
		"Sec-Fetch-Site": "same-origin",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("new token status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func TestSignOutAllInvalidatesTokens(t *testing.T) {
	h := newAccountHandler(t, mgmtauth.SystemClock{})
	engine := newTestEngine(h)

	loginRec := doRequest(engine, http.MethodPost, "/v8/management/session/login", loginJSON("admin", testAccountPassword), nil)
	cookieHeader, _ := sessionCookieFrom(loginRec)
	headers := map[string]string{"Cookie": cookieHeader, "Sec-Fetch-Site": "same-origin"}

	rec := doRequest(engine, http.MethodPost, "/v8/management/account/sign-out-all", "", headers)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("sign-out-all status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}

	rec = doRequest(engine, http.MethodGet, "/v8/management/account", "", headers)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("post-sign-out-all status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

func TestGetAccountView(t *testing.T) {
	h := newAccountHandler(t, mgmtauth.SystemClock{})
	engine := newTestEngine(h)

	loginRec := doRequest(engine, http.MethodPost, "/v8/management/session/login", loginJSON("admin", testAccountPassword), nil)
	cookieHeader, _ := sessionCookieFrom(loginRec)

	rec := doRequest(engine, http.MethodGet, "/v8/management/account", "", map[string]string{"Cookie": cookieHeader, "Sec-Fetch-Site": "same-origin"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Configured bool   `json:"configured"`
		Username   string `json:"username"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.Configured || body.Username != "admin" {
		t.Fatalf("account view = %+v, want configured=true username=admin", body)
	}
}

// TestGetAccountEffectivePasskeyOrigins mirrors the session/status
// behavior: GET /account must also report the effective origin list.
func TestGetAccountEffectivePasskeyOrigins(t *testing.T) {
	h := newAccountHandler(t, mgmtauth.SystemClock{})
	h.mu.Lock()
	h.cfg.RemoteManagement.Login.PasskeyRPID = "mgmt.example.com"
	h.mu.Unlock()
	engine := newTestEngine(h)

	loginRec := doRequest(engine, http.MethodPost, "/v8/management/session/login", loginJSON("admin", testAccountPassword), nil)
	cookieHeader, _ := sessionCookieFrom(loginRec)

	rec := doRequest(engine, http.MethodGet, "/v8/management/account", "", map[string]string{"Cookie": cookieHeader, "Sec-Fetch-Site": "same-origin"})
	var body struct {
		PasskeyRPID    string   `json:"passkey_rp_id"`
		PasskeyOrigins []string `json:"passkey_origins"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.PasskeyRPID != "mgmt.example.com" {
		t.Fatalf("passkey_rp_id = %q, want mgmt.example.com", body.PasskeyRPID)
	}
	if len(body.PasskeyOrigins) != 1 || body.PasskeyOrigins[0] != "https://mgmt.example.com" {
		t.Fatalf("passkey_origins = %v, want [https://mgmt.example.com] (defaulted)", body.PasskeyOrigins)
	}
}

// TestPasskeyRegistrationAndLoginOverHTTP drives /account/passkeys/begin+finish
// then /session/passkey/begin+finish end to end using a software ES256
// authenticator, through the real HTTP handlers.
func TestPasskeyRegistrationAndLoginOverHTTP(t *testing.T) {
	const rpID = "mgmt.example.com"
	const origin = "https://mgmt.example.com"

	h := newAccountHandler(t, mgmtauth.SystemClock{})
	h.mu.Lock()
	h.cfg.RemoteManagement.Login.PasskeyRPID = rpID
	h.cfg.RemoteManagement.Login.PasskeyOrigins = []string{origin}
	h.mu.Unlock()
	engine := newTestEngine(h)

	loginRec := doRequest(engine, http.MethodPost, "/v8/management/session/login", loginJSON("admin", testAccountPassword), nil)
	cookieHeader, _ := sessionCookieFrom(loginRec)
	authedHeaders := map[string]string{"Cookie": cookieHeader, "Sec-Fetch-Site": "same-origin"}

	beginRec := doRequest(engine, http.MethodPost, "/v8/management/account/passkeys/begin", "", authedHeaders)
	if beginRec.Code != http.StatusOK {
		t.Fatalf("passkeys/begin status = %d, want 200; body=%s", beginRec.Code, beginRec.Body.String())
	}
	var beginResp struct {
		CeremonyID string `json:"ceremony_id"`
		Options    struct {
			PublicKey struct {
				Challenge string `json:"challenge"`
			} `json:"publicKey"`
		} `json:"options"`
	}
	if err := json.Unmarshal(beginRec.Body.Bytes(), &beginResp); err != nil {
		t.Fatalf("decode begin response: %v", err)
	}

	authenticator := newSoftAuthenticator(t, []byte("http-credential-1"))
	challengeBytes := decodeB64URLForTest(t, beginResp.Options.PublicKey.Challenge)
	regJSON := authenticator.registrationResponseJSON(t, rpID, origin, challengeBytes)

	finishBody, _ := json.Marshal(map[string]interface{}{
		"ceremony_id": beginResp.CeremonyID,
		"name":        "Test Key",
		"credential":  json.RawMessage(regJSON),
	})
	finishRec := doRequest(engine, http.MethodPost, "/v8/management/account/passkeys/finish", string(finishBody), authedHeaders)
	if finishRec.Code != http.StatusOK {
		t.Fatalf("passkeys/finish status = %d, want 200; body=%s", finishRec.Code, finishRec.Body.String())
	}

	// Now log in with the passkey via the public session routes.
	loginBeginRec := doRequest(engine, http.MethodPost, "/v8/management/session/passkey/begin", "", nil)
	if loginBeginRec.Code != http.StatusOK {
		t.Fatalf("session passkey/begin status = %d, want 200; body=%s", loginBeginRec.Code, loginBeginRec.Body.String())
	}
	var loginBeginResp struct {
		CeremonyID string `json:"ceremony_id"`
		Options    struct {
			PublicKey struct {
				Challenge string `json:"challenge"`
			} `json:"publicKey"`
		} `json:"options"`
	}
	if err := json.Unmarshal(loginBeginRec.Body.Bytes(), &loginBeginResp); err != nil {
		t.Fatalf("decode login-begin response: %v", err)
	}

	loginChallenge := decodeB64URLForTest(t, loginBeginResp.Options.PublicKey.Challenge)
	userHandle := decodeB64URLForTest(t, h.currentLoginConfig().UserHandle)
	assertionJSON := authenticator.assertionResponseJSON(t, rpID, origin, loginChallenge, userHandle)

	loginFinishBody, _ := json.Marshal(map[string]interface{}{
		"ceremony_id": loginBeginResp.CeremonyID,
		"credential":  json.RawMessage(assertionJSON),
	})
	loginFinishRec := doRequest(engine, http.MethodPost, "/v8/management/session/passkey/finish", string(loginFinishBody), nil)
	if loginFinishRec.Code != http.StatusOK {
		t.Fatalf("session passkey/finish status = %d, want 200; body=%s", loginFinishRec.Code, loginFinishRec.Body.String())
	}
	var passkeyLoginResp struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(loginFinishRec.Body.Bytes(), &passkeyLoginResp)
	if !strings.HasPrefix(passkeyLoginResp.Token, mgmtauth.TokenPrefix) {
		t.Fatalf("passkey login token = %q, want cpas_ prefix", passkeyLoginResp.Token)
	}
}

// TestPasskeyLoginRejectsOriginOutsidePasskeyOrigins mirrors the mgmtauth-level
// coverage at the HTTP layer: an assertion built for an origin absent from
// passkey-origins must be rejected by /session/passkey/finish.
func TestPasskeyLoginRejectsOriginOutsidePasskeyOrigins(t *testing.T) {
	const rpID = "mgmt.example.com"
	const allowedOrigin = "https://allowed.example.com"
	const disallowedOrigin = "https://not-allowed.example.com"

	h := newAccountHandler(t, mgmtauth.SystemClock{})
	h.mu.Lock()
	h.cfg.RemoteManagement.Login.PasskeyRPID = rpID
	h.cfg.RemoteManagement.Login.PasskeyOrigins = []string{allowedOrigin}
	h.mu.Unlock()
	engine := newTestEngine(h)

	loginRec := doRequest(engine, http.MethodPost, "/v8/management/session/login", loginJSON("admin", testAccountPassword), nil)
	cookieHeader, _ := sessionCookieFrom(loginRec)
	authedHeaders := map[string]string{"Cookie": cookieHeader, "Sec-Fetch-Site": "same-origin"}

	beginRec := doRequest(engine, http.MethodPost, "/v8/management/account/passkeys/begin", "", authedHeaders)
	var beginResp struct {
		CeremonyID string `json:"ceremony_id"`
		Options    struct {
			PublicKey struct {
				Challenge string `json:"challenge"`
			} `json:"publicKey"`
		} `json:"options"`
	}
	_ = json.Unmarshal(beginRec.Body.Bytes(), &beginResp)

	authenticator := newSoftAuthenticator(t, []byte("http-credential-2"))
	regJSON := authenticator.registrationResponseJSON(t, rpID, allowedOrigin, decodeB64URLForTest(t, beginResp.Options.PublicKey.Challenge))
	finishBody, _ := json.Marshal(map[string]interface{}{
		"ceremony_id": beginResp.CeremonyID,
		"name":        "Test Key",
		"credential":  json.RawMessage(regJSON),
	})
	doRequest(engine, http.MethodPost, "/v8/management/account/passkeys/finish", string(finishBody), authedHeaders)

	loginBeginRec := doRequest(engine, http.MethodPost, "/v8/management/session/passkey/begin", "", nil)
	var loginBeginResp struct {
		CeremonyID string `json:"ceremony_id"`
		Options    struct {
			PublicKey struct {
				Challenge string `json:"challenge"`
			} `json:"publicKey"`
		} `json:"options"`
	}
	_ = json.Unmarshal(loginBeginRec.Body.Bytes(), &loginBeginResp)

	userHandle := decodeB64URLForTest(t, h.currentLoginConfig().UserHandle)
	loginChallenge := decodeB64URLForTest(t, loginBeginResp.Options.PublicKey.Challenge)
	// Assert from a disallowed origin.
	assertionJSON := authenticator.assertionResponseJSON(t, rpID, disallowedOrigin, loginChallenge, userHandle)
	loginFinishBody, _ := json.Marshal(map[string]interface{}{
		"ceremony_id": loginBeginResp.CeremonyID,
		"credential":  json.RawMessage(assertionJSON),
	})
	loginFinishRec := doRequest(engine, http.MethodPost, "/v8/management/session/passkey/finish", string(loginFinishBody), nil)
	if loginFinishRec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for a disallowed origin; body=%s", loginFinishRec.Code, loginFinishRec.Body.String())
	}
}

func decodeB64URLForTest(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("decode base64url %q: %v", s, err)
	}
	return b
}
