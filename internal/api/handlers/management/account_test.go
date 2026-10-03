package management

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/mgmtauth"
)

func putAccountBody(username, password, currentPassword string) string {
	b, _ := json.Marshal(map[string]string{
		"username":         username,
		"password":         password,
		"current_password": currentPassword,
	})
	return string(b)
}

func currentPasswordBody(currentPassword string) string {
	b, _ := json.Marshal(map[string]string{"current_password": currentPassword})
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

	account := h.loginStore.Get()
	if account == nil || account.Username != "admin" || account.PasswordHash == "" {
		t.Fatalf("account not persisted: %+v", account)
	}
	if account.SessionSecret == "" || account.UserHandle == "" {
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
	h.allowRemoteOverride = true
	engine := newTestEngine(h)

	rec := doRequest(engine, http.MethodPut, "/v8/management/account", putAccountBody("renamed-admin", "", ""), map[string]string{
		"X-Management-Key": "test-secret",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	account := h.loginStore.Get()
	if account.Username != "renamed-admin" {
		t.Fatalf("username = %q, want renamed-admin", account.Username)
	}
	ok, err := mgmtauth.VerifyPassword(account.PasswordHash, testAccountPassword)
	if err != nil || !ok {
		t.Fatalf("expected the original password to still verify, ok=%v err=%v", ok, err)
	}
}

// TestPutAccountUsernameOnlyChangeOverSessionRequiresCurrentPassword covers
// the rule that current_password is required for ANY change over a
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
	if h.loginStore.Get().Username != "renamed-admin" {
		t.Fatalf("username = %q, want renamed-admin", h.loginStore.Get().Username)
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

	rec := doRequest(engine, http.MethodPut, "/v8/management/account", putAccountBody("admin", "a-new-strong-password", ""), headers)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("missing current_password status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}

	rec = doRequest(engine, http.MethodPut, "/v8/management/account", putAccountBody("admin", "a-new-strong-password", "nope-not-it"), headers)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("wrong current_password status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

func TestPutAccountOverKeyDoesNotRequireCurrentPassword(t *testing.T) {
	h := newAccountHandler(t, mgmtauth.SystemClock{})
	h.envSecret = "test-secret"
	h.allowRemoteOverride = true
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

	// The old token must now be rejected outright (401 "session expired"),
	// since it is a presented-but-invalid cpas_ credential, not a missing one.
	rec = doRequest(engine, http.MethodGet, "/v8/management/account", "", map[string]string{
		"Cookie":         oldCookieHeader,
		"Sec-Fetch-Site": "same-origin",
	})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("old token status = %d, want 401; body=%s", rec.Code, rec.Body.String())
	}

	rec = doRequest(engine, http.MethodGet, "/v8/management/account", "", map[string]string{
		"Cookie":         newCookieHeader,
		"Sec-Fetch-Site": "same-origin",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("new token status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// TestPutAccountResponseKeepsPasskeyMethodLabel verifies a caller who is
// authenticated via a passkey session keeps that method label on the fresh
// session PUT /account issues, rather than resetting it to password.
func TestPutAccountResponseKeepsPasskeyMethodLabel(t *testing.T) {
	const rpID = "mgmt.example.com"
	const origin = "https://mgmt.example.com"

	h := newAccountHandler(t, mgmtauth.SystemClock{})
	if _, err := h.loginStore.Mutate(func(acct *mgmtauth.Account) (*mgmtauth.Account, error) {
		acct.PasskeyRPID = rpID
		acct.PasskeyOrigins = []string{origin}
		return acct, nil
	}); err != nil {
		t.Fatalf("seed passkey settings: %v", err)
	}
	engine := newTestEngine(h)

	// Register a passkey over a password session first.
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
	_ = json.Unmarshal(beginRec.Body.Bytes(), &beginResp)

	authenticator := newSoftAuthenticator(t, []byte("method-label-credential"))
	regJSON := authenticator.registrationResponseJSON(t, rpID, origin, decodeB64URLForTest(t, beginResp.Options.PublicKey.Challenge))
	finishBody, _ := json.Marshal(map[string]interface{}{
		"ceremony_id": beginResp.CeremonyID,
		"name":        "Test Key",
		"credential":  json.RawMessage(regJSON),
	})
	doRequest(engine, http.MethodPost, "/v8/management/account/passkeys/finish", string(finishBody), authedHeaders)

	// Log in again, this time via the passkey.
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
	userHandle := decodeB64URLForTest(t, h.loginStore.Get().UserHandle)
	assertionJSON := authenticator.assertionResponseJSON(t, rpID, origin, decodeB64URLForTest(t, loginBeginResp.Options.PublicKey.Challenge), userHandle)
	loginFinishBody, _ := json.Marshal(map[string]interface{}{
		"ceremony_id": loginBeginResp.CeremonyID,
		"credential":  json.RawMessage(assertionJSON),
	})
	passkeyLoginRec := doRequest(engine, http.MethodPost, "/v8/management/session/passkey/finish", string(loginFinishBody), nil)
	passkeyCookieHeader, _ := sessionCookieFrom(passkeyLoginRec)

	// Confirm this session's method is passkey before doing a username-only PUT.
	statusRec := doRequest(engine, http.MethodGet, "/v8/management/session/status", "", map[string]string{"Cookie": passkeyCookieHeader})
	var status struct {
		Method string `json:"method"`
	}
	_ = json.Unmarshal(statusRec.Body.Bytes(), &status)
	if status.Method != "passkey" {
		t.Fatalf("precondition: session method = %q, want passkey", status.Method)
	}

	putHeaders := map[string]string{"Cookie": passkeyCookieHeader, "Sec-Fetch-Site": "same-origin"}
	putRec := doRequest(engine, http.MethodPut, "/v8/management/account", putAccountBody("renamed-admin", "", testAccountPassword), putHeaders)
	if putRec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200; body=%s", putRec.Code, putRec.Body.String())
	}
	var putResp struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(putRec.Body.Bytes(), &putResp)

	finalStatusRec := doRequest(engine, http.MethodGet, "/v8/management/session/status", "", map[string]string{
		"Authorization": "Bearer " + putResp.Token,
	})
	_ = json.Unmarshal(finalStatusRec.Body.Bytes(), &status)
	if status.Method != "passkey" {
		t.Fatalf("session method after PUT = %q, want passkey (caller's method label should be kept)", status.Method)
	}
}

// TestPutAccountBearerDoesNotEmitStaleRefreshHeader covers: a password
// change over bearer must not also carry an X-CPA-Session-Refresh header
// signed with the old (now-rotated) secret.
func TestPutAccountBearerDoesNotEmitStaleRefreshHeader(t *testing.T) {
	clock := newMockClock(time.Now())
	h := newAccountHandler(t, clock)
	engine := newTestEngine(h)

	loginRec := doRequest(engine, http.MethodPost, "/v8/management/session/login", loginJSON("admin", testAccountPassword), nil)
	var loginResp struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(loginRec.Body.Bytes(), &loginResp)

	// Advance past the halfway point so Middleware() would normally slide
	// the bearer token forward with the OLD secret on this very request.
	clock.Advance(mgmtauth.DefaultLifetime/2 + time.Minute)

	rec := doRequest(engine, http.MethodPut, "/v8/management/account", putAccountBody("admin", "a-new-strong-password", testAccountPassword), map[string]string{
		"Authorization": "Bearer " + loginResp.Token,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var putResp struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &putResp)

	refreshHeader := rec.Header().Get("X-CPA-Session-Refresh")
	if refreshHeader != "" {
		t.Fatalf("X-CPA-Session-Refresh = %q, want empty (no stale refresh alongside the fresh token)", refreshHeader)
	}
	if putResp.Token == "" {
		t.Fatal("expected a fresh token in the response body")
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
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("post-sign-out-all status = %d, want 401; body=%s", rec.Code, rec.Body.String())
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
	if _, err := h.loginStore.Mutate(func(acct *mgmtauth.Account) (*mgmtauth.Account, error) {
		acct.PasskeyRPID = "mgmt.example.com"
		return acct, nil
	}); err != nil {
		t.Fatalf("seed rp-id: %v", err)
	}
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
	if _, err := h.loginStore.Mutate(func(acct *mgmtauth.Account) (*mgmtauth.Account, error) {
		acct.PasskeyRPID = rpID
		acct.PasskeyOrigins = []string{origin}
		return acct, nil
	}); err != nil {
		t.Fatalf("seed passkey settings: %v", err)
	}
	engine := newTestEngine(h)

	loginRec := doRequest(engine, http.MethodPost, "/v8/management/session/login", loginJSON("admin", testAccountPassword), nil)
	cookieHeader, _ := sessionCookieFrom(loginRec)
	authedHeaders := map[string]string{"Cookie": cookieHeader, "Sec-Fetch-Site": "same-origin"}

	beginRec := doRequest(engine, http.MethodPost, "/v8/management/account/passkeys/begin", currentPasswordBody(testAccountPassword), authedHeaders)
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

	// passkeys_available must now be true, and the stored passkey must carry
	// the current rp-id.
	statusRec := doRequest(engine, http.MethodGet, "/v8/management/session/status", "", nil)
	var status struct {
		PasskeysAvailable bool `json:"passkeys_available"`
	}
	_ = json.Unmarshal(statusRec.Body.Bytes(), &status)
	if !status.PasskeysAvailable {
		t.Fatal("expected passkeys_available = true after registering a passkey")
	}

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
	userHandle := decodeB64URLForTest(t, h.loginStore.Get().UserHandle)
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
	if _, err := h.loginStore.Mutate(func(acct *mgmtauth.Account) (*mgmtauth.Account, error) {
		acct.PasskeyRPID = rpID
		acct.PasskeyOrigins = []string{allowedOrigin}
		return acct, nil
	}); err != nil {
		t.Fatalf("seed passkey settings: %v", err)
	}
	engine := newTestEngine(h)

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

	userHandle := decodeB64URLForTest(t, h.loginStore.Get().UserHandle)
	loginChallenge := decodeB64URLForTest(t, loginBeginResp.Options.PublicKey.Challenge)
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

// TestPasskeysAvailableOnlyCountsCurrentRPID covers: changing rp-id must
// make an old passkey registered under the previous rp-id no longer count
// toward passkeys_available.
func TestPasskeysAvailableOnlyCountsCurrentRPID(t *testing.T) {
	h := newAccountHandler(t, mgmtauth.SystemClock{})
	if _, err := h.loginStore.Mutate(func(acct *mgmtauth.Account) (*mgmtauth.Account, error) {
		acct.PasskeyRPID = "old.example.com"
		acct.Passkeys = []mgmtauth.PasskeyRecord{{ID: "old-cred", PublicKey: "pub", RPID: "old.example.com"}}
		return acct, nil
	}); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	engine := newTestEngine(h)

	rec := doRequest(engine, http.MethodGet, "/v8/management/session/status", "", nil)
	var status struct {
		PasskeysAvailable bool `json:"passkeys_available"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &status)
	if !status.PasskeysAvailable {
		t.Fatal("expected passkeys_available = true while rp-id matches the stored passkey")
	}

	if _, err := h.loginStore.Mutate(func(acct *mgmtauth.Account) (*mgmtauth.Account, error) {
		acct.PasskeyRPID = "new.example.com"
		return acct, nil
	}); err != nil {
		t.Fatalf("change rp-id: %v", err)
	}

	rec = doRequest(engine, http.MethodGet, "/v8/management/session/status", "", nil)
	_ = json.Unmarshal(rec.Body.Bytes(), &status)
	if status.PasskeysAvailable {
		t.Fatal("expected passkeys_available = false once rp-id no longer matches the stored passkey")
	}
}

// TestPasskeyCeremonyBeginNeverRejectsAtCapacity covers the ceremony cap at
// the HTTP layer: once MaxPendingCeremonies login ceremonies are
// outstanding, begin still succeeds (evicting the oldest) rather than
// returning 429 -- the eviction mechanics themselves are covered by
// mgmtauth's own TestCeremonyCacheCapsPendingCeremoniesByEvictingOldest.
func TestPasskeyCeremonyBeginNeverRejectsAtCapacity(t *testing.T) {
	h := newAccountHandler(t, mgmtauth.SystemClock{})
	if _, err := h.loginStore.Mutate(func(acct *mgmtauth.Account) (*mgmtauth.Account, error) {
		acct.PasskeyRPID = "mgmt.example.com"
		acct.Passkeys = []mgmtauth.PasskeyRecord{{ID: "cred", PublicKey: "pub", RPID: "mgmt.example.com"}}
		return acct, nil
	}); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	engine := newTestEngine(h)

	for i := 0; i < mgmtauth.MaxPendingCeremonies; i++ {
		rec := doRequest(engine, http.MethodPost, "/v8/management/session/passkey/begin", "", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("begin #%d: status = %d, want 200; body=%s", i, rec.Code, rec.Body.String())
		}
	}
	rec := doRequest(engine, http.MethodPost, "/v8/management/session/passkey/begin", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("begin at capacity: status = %d, want 200 (evict-oldest, not reject); body=%s", rec.Code, rec.Body.String())
	}
}

// TestPostAccountPasskeysBeginRequiresCurrentPasswordOverSession and
// TestPutAccountPasskeySettingsRequiresCurrentPasswordOverSession cover S7:
// both endpoints require the correct current_password over a session, and
// not at all over the key.
func TestPostAccountPasskeysBeginRequiresCurrentPasswordOverSession(t *testing.T) {
	clock := newMockClock(time.Now())
	h := newAccountHandler(t, clock)
	if _, err := h.loginStore.Mutate(func(acct *mgmtauth.Account) (*mgmtauth.Account, error) {
		acct.PasskeyRPID = "mgmt.example.com"
		return acct, nil
	}); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	engine := newTestEngine(h)

	loginRec := doRequest(engine, http.MethodPost, "/v8/management/session/login", loginJSON("admin", testAccountPassword), nil)
	cookieHeader, _ := sessionCookieFrom(loginRec)
	headers := map[string]string{"Cookie": cookieHeader, "Sec-Fetch-Site": "same-origin"}

	rec := doRequest(engine, http.MethodPost, "/v8/management/account/passkeys/begin", "", headers)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("missing current_password status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	rec = doRequest(engine, http.MethodPost, "/v8/management/account/passkeys/begin", currentPasswordBody("wrong"), headers)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("wrong current_password status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	clock.Advance(time.Minute) // clear the failure backoff window
	rec = doRequest(engine, http.MethodPost, "/v8/management/account/passkeys/begin", currentPasswordBody(testAccountPassword), headers)
	if rec.Code != http.StatusOK {
		t.Fatalf("correct current_password status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func TestPostAccountPasskeysBeginOverKeyDoesNotRequireCurrentPassword(t *testing.T) {
	h := newAccountHandler(t, mgmtauth.SystemClock{})
	h.envSecret = "test-secret"
	h.allowRemoteOverride = true
	if _, err := h.loginStore.Mutate(func(acct *mgmtauth.Account) (*mgmtauth.Account, error) {
		acct.PasskeyRPID = "mgmt.example.com"
		return acct, nil
	}); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	engine := newTestEngine(h)

	rec := doRequest(engine, http.MethodPost, "/v8/management/account/passkeys/begin", "", map[string]string{"X-Management-Key": "test-secret"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func TestPutAccountPasskeySettingsRequiresCurrentPasswordOverSession(t *testing.T) {
	h := newAccountHandler(t, mgmtauth.SystemClock{})
	engine := newTestEngine(h)

	loginRec := doRequest(engine, http.MethodPost, "/v8/management/session/login", loginJSON("admin", testAccountPassword), nil)
	cookieHeader, _ := sessionCookieFrom(loginRec)
	headers := map[string]string{"Cookie": cookieHeader, "Sec-Fetch-Site": "same-origin"}

	body, _ := json.Marshal(map[string]interface{}{"rp_id": "mgmt.example.com", "origins": []string{"https://mgmt.example.com"}})
	rec := doRequest(engine, http.MethodPut, "/v8/management/account/passkey-settings", string(body), headers)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("missing current_password status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}

	body, _ = json.Marshal(map[string]interface{}{"rp_id": "mgmt.example.com", "origins": []string{"https://mgmt.example.com"}, "current_password": testAccountPassword})
	rec = doRequest(engine, http.MethodPut, "/v8/management/account/passkey-settings", string(body), headers)
	if rec.Code != http.StatusOK {
		t.Fatalf("correct current_password status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func TestPutAccountPasskeySettingsOverKeyDoesNotRequireCurrentPassword(t *testing.T) {
	h := newAccountHandler(t, mgmtauth.SystemClock{})
	h.envSecret = "test-secret"
	h.allowRemoteOverride = true
	engine := newTestEngine(h)

	body, _ := json.Marshal(map[string]interface{}{"rp_id": "mgmt.example.com", "origins": []string{"https://mgmt.example.com"}})
	rec := doRequest(engine, http.MethodPut, "/v8/management/account/passkey-settings", string(body), map[string]string{"X-Management-Key": "test-secret"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// TestPostAccountPasskeysFinishRejectsDuplicateCredential covers N11.
func TestPostAccountPasskeysFinishRejectsDuplicateCredential(t *testing.T) {
	const rpID = "mgmt.example.com"
	const origin = "https://mgmt.example.com"

	h := newAccountHandler(t, mgmtauth.SystemClock{})
	if _, err := h.loginStore.Mutate(func(acct *mgmtauth.Account) (*mgmtauth.Account, error) {
		acct.PasskeyRPID = rpID
		acct.PasskeyOrigins = []string{origin}
		return acct, nil
	}); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	engine := newTestEngine(h)

	loginRec := doRequest(engine, http.MethodPost, "/v8/management/session/login", loginJSON("admin", testAccountPassword), nil)
	cookieHeader, _ := sessionCookieFrom(loginRec)
	authedHeaders := map[string]string{"Cookie": cookieHeader, "Sec-Fetch-Site": "same-origin"}

	authenticator := newSoftAuthenticator(t, []byte("dup-credential"))

	register := func() *httptest.ResponseRecorder {
		beginRec := doRequest(engine, http.MethodPost, "/v8/management/account/passkeys/begin", currentPasswordBody(testAccountPassword), authedHeaders)
		var beginResp struct {
			CeremonyID string `json:"ceremony_id"`
			Options    struct {
				PublicKey struct {
					Challenge string `json:"challenge"`
				} `json:"publicKey"`
			} `json:"options"`
		}
		_ = json.Unmarshal(beginRec.Body.Bytes(), &beginResp)
		regJSON := authenticator.registrationResponseJSON(t, rpID, origin, decodeB64URLForTest(t, beginResp.Options.PublicKey.Challenge))
		finishBody, _ := json.Marshal(map[string]interface{}{
			"ceremony_id": beginResp.CeremonyID,
			"name":        "Dup Key",
			"credential":  json.RawMessage(regJSON),
		})
		return doRequest(engine, http.MethodPost, "/v8/management/account/passkeys/finish", string(finishBody), authedHeaders)
	}

	first := register()
	if first.Code != http.StatusOK {
		t.Fatalf("first registration status = %d, want 200; body=%s", first.Code, first.Body.String())
	}
	second := register()
	if second.Code != http.StatusConflict {
		t.Fatalf("duplicate registration status = %d, want 409; body=%s", second.Code, second.Body.String())
	}
}

// TestPostAccountPasskeysFinishStatusCodes covers item A: ceremony/
// verification failures on this authenticated route use 410/400, never
// 401 (which the panel treats as "logged out").
func TestPostAccountPasskeysFinishStatusCodes(t *testing.T) {
	h := newAccountHandler(t, mgmtauth.SystemClock{})
	if _, err := h.loginStore.Mutate(func(acct *mgmtauth.Account) (*mgmtauth.Account, error) {
		acct.PasskeyRPID = "mgmt.example.com"
		return acct, nil
	}); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	engine := newTestEngine(h)

	loginRec := doRequest(engine, http.MethodPost, "/v8/management/session/login", loginJSON("admin", testAccountPassword), nil)
	cookieHeader, _ := sessionCookieFrom(loginRec)
	authedHeaders := map[string]string{"Cookie": cookieHeader, "Sec-Fetch-Site": "same-origin"}

	// Unknown/expired ceremony -> 410, not 401.
	body, _ := json.Marshal(map[string]interface{}{"ceremony_id": "does-not-exist", "name": "x", "credential": json.RawMessage(`{}`)})
	rec := doRequest(engine, http.MethodPost, "/v8/management/account/passkeys/finish", string(body), authedHeaders)
	if rec.Code != http.StatusGone {
		t.Fatalf("unknown ceremony status = %d, want 410; body=%s", rec.Code, rec.Body.String())
	}

	// A real ceremony with garbage credential data -> 400, not 401.
	beginRec := doRequest(engine, http.MethodPost, "/v8/management/account/passkeys/begin", currentPasswordBody(testAccountPassword), authedHeaders)
	var beginResp struct {
		CeremonyID string `json:"ceremony_id"`
	}
	_ = json.Unmarshal(beginRec.Body.Bytes(), &beginResp)
	badBody, _ := json.Marshal(map[string]interface{}{"ceremony_id": beginResp.CeremonyID, "name": "x", "credential": json.RawMessage(`{"garbage":true}`)})
	rec = doRequest(engine, http.MethodPost, "/v8/management/account/passkeys/finish", string(badBody), authedHeaders)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed credential status = %d, want 400; body=%s", rec.Code, rec.Body.String())
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
