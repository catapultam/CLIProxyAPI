package management

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/mgmtauth"
	log "github.com/sirupsen/logrus"
)

// SessionCookieName is the HttpOnly cookie carrying a signed session token
// for same-origin panel clients.
const SessionCookieName = "cpa_mgmt_session"

// sessionCookiePath scopes the cookie to the v8 management API the panel
// actually uses, rather than the whole site.
const sessionCookiePath = "/v8/management"

// AuthMethodContextKey records how the current request authenticated, for
// handlers (notably PUT /account) that must behave differently for a session
// versus a management key.
const AuthMethodContextKey = "management.auth-method"

// LoginMethodContextKey records the mgmtauth.Method ("password" or
// "passkey") a session-authenticated request used, set alongside
// AuthMethodContextKey when AuthMethodSession applies.
const LoginMethodContextKey = "management.login-method"

// Values stored under AuthMethodContextKey.
const (
	AuthMethodSession = "session"
	AuthMethodKey     = "key"
)

// sessionRPDisplayName is the WebAuthn relying party display name shown by
// browser/OS passkey UI.
const sessionRPDisplayName = "CLIProxyAPI"

// errUnknownUser is returned by the discoverable-login lookup when the
// assertion's user handle does not match the single configured account.
var errUnknownUser = errors.New("management: unknown passkey user")

// sessionAuthStatus is the outcome of trying to authenticate a request via
// the session cookie/bearer token, before falling back to the management key.
type sessionAuthStatus int

const (
	// sessionAuthNone means no cpas_ credential was presented at all; the
	// caller should fall through to the management-key check.
	sessionAuthNone sessionAuthStatus = iota
	// sessionAuthOK means the request is authenticated via session.
	sessionAuthOK
	// sessionAuthCSRFBlocked means a valid cookie session was presented but
	// the CSRF guard rejected the request outright.
	sessionAuthCSRFBlocked
	// sessionAuthInvalid means a cpas_ credential (cookie and/or bearer) was
	// presented but none of them verified. This must not fall through to
	// key auth and must not count against the key's failure bookkeeping.
	sessionAuthInvalid
)

// tryAuthenticateSession implements the first step of Middleware(): verify a
// cpa_mgmt_session cookie and/or an Authorization: Bearer cpas_... token
// (accepting whichever one is valid when both are present -- the bearer is
// tried first, so a cookie that fails the CSRF guard can never block an
// otherwise-valid bearer), apply the CSRF guard for cookie-authenticated
// requests, and slide the session forward when it is past its halfway
// point. When every presented credential fails and a cookie was among
// them, the cookie is cleared in the response: a cpas_ cookie the browser
// is still holding is, by definition, no longer any good.
func (h *Handler) tryAuthenticateSession(c *gin.Context) sessionAuthStatus {
	if h == nil {
		return sessionAuthNone
	}

	cookieToken, hasCookie := sessionCookieToken(c)
	bearerToken, hasBearer := sessionBearerToken(c)
	if !hasCookie && !hasBearer {
		return sessionAuthNone
	}

	account := h.loginStore.Get()
	origins := effectivePasskeyOrigins(account)
	var secret []byte
	if mgmtauth.HasAccount(account) {
		secret, _ = decodeLoginSecret(account.SessionSecret)
	}
	now := h.now()

	type candidate struct {
		token     string
		viaCookie bool
	}
	var candidates []candidate
	if hasBearer {
		candidates = append(candidates, candidate{bearerToken, false})
	}
	if hasCookie {
		candidates = append(candidates, candidate{cookieToken, true})
	}

	for _, cand := range candidates {
		if len(secret) == 0 {
			continue
		}
		claims, err := mgmtauth.VerifyToken(secret, cand.token, now)
		if err != nil {
			continue
		}
		if cand.viaCookie && !csrfAllowed(c, origins) {
			return sessionAuthCSRFBlocked
		}

		c.Set(LoginMethodContextKey, string(claims.Method))
		if mgmtauth.ShouldRefresh(claims, now, mgmtauth.DefaultLifetime) {
			if refreshed, expiresAt, errIssue := mgmtauth.IssueToken(secret, claims.Method, now, mgmtauth.DefaultLifetime); errIssue == nil {
				if cand.viaCookie {
					h.setSessionCookie(c, refreshed, expiresAt, origins)
				} else {
					c.Header("X-CPA-Session-Refresh", refreshed)
				}
			}
		}
		return sessionAuthOK
	}

	// Every presented cpas_ credential failed to verify.
	if hasCookie {
		h.clearSessionCookie(c, origins)
	}
	return sessionAuthInvalid
}

// sessionCookieToken extracts a cpas_-prefixed token from the session
// cookie, if present.
func sessionCookieToken(c *gin.Context) (string, bool) {
	cookie, err := c.Cookie(SessionCookieName)
	if err != nil || !strings.HasPrefix(cookie, mgmtauth.TokenPrefix) {
		return "", false
	}
	return cookie, true
}

// sessionBearerToken extracts a cpas_-prefixed token from an
// Authorization: Bearer header, if present.
func sessionBearerToken(c *gin.Context) (string, bool) {
	ah := c.GetHeader("Authorization")
	if ah == "" {
		return "", false
	}
	parts := strings.SplitN(ah, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "bearer") || !strings.HasPrefix(parts[1], mgmtauth.TokenPrefix) {
		return "", false
	}
	return parts[1], true
}

func isUnsafeMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	default:
		return true
	}
}

// csrfAllowed implements the CSRF guard for cookie-authenticated requests.
// Sec-Fetch-Site, when present, governs regardless of method: only
// same-origin and none are allowed, and same-site/cross-site are rejected
// even for a safe method like GET. When Sec-Fetch-Site is absent, safe
// methods are allowed outright; unsafe methods fall back to the Origin
// header, which must equal the request Host or be one of the configured
// passkey origins -- an absent or "null" Origin is a mismatch.
func csrfAllowed(c *gin.Context, originsAllowed []string) bool {
	if sfs := c.GetHeader("Sec-Fetch-Site"); sfs != "" {
		return sfs == "same-origin" || sfs == "none"
	}
	if !isUnsafeMethod(c.Request.Method) {
		return true
	}
	origin := strings.TrimSpace(c.GetHeader("Origin"))
	if origin == "" || strings.EqualFold(origin, "null") {
		return false
	}
	if u, err := url.Parse(origin); err == nil && strings.EqualFold(u.Host, c.Request.Host) {
		return true
	}
	trimmed := strings.TrimRight(origin, "/")
	for _, allowed := range originsAllowed {
		if strings.EqualFold(strings.TrimRight(strings.TrimSpace(allowed), "/"), trimmed) {
			return true
		}
	}
	return false
}

// isHTTPSRequest reports whether the current request should be treated as
// HTTPS for the session cookie's Secure attribute: direct TLS,
// X-Forwarded-Proto: https, an https Origin or Referer, or a request Host
// that equals the host:port of one of the configured (https) passkey
// origins -- the last case keeps Secure set on a sliding-refresh GET made
// directly against an HTTPS origin like cakebox's :8443 tailscale serve,
// which carries neither Origin nor Referer.
func isHTTPSRequest(c *gin.Context, allowedOrigins []string) bool {
	// The server's protocol-sniffing bufferedConn always implements
	// ConnectionState, so net/http sets a zero-value Request.TLS even on plain
	// HTTP connections; only a completed handshake means real TLS.
	if c.Request.TLS != nil && c.Request.TLS.HandshakeComplete {
		return true
	}
	if proto := c.GetHeader("X-Forwarded-Proto"); strings.EqualFold(strings.TrimSpace(proto), "https") {
		return true
	}
	if origin := c.GetHeader("Origin"); origin != "" {
		if u, err := url.Parse(origin); err == nil && strings.EqualFold(u.Scheme, "https") {
			return true
		}
	}
	if referer := c.GetHeader("Referer"); referer != "" {
		if u, err := url.Parse(referer); err == nil && strings.EqualFold(u.Scheme, "https") {
			return true
		}
	}
	host := c.Request.Host
	for _, o := range allowedOrigins {
		if u, err := url.Parse(strings.TrimSpace(o)); err == nil && strings.EqualFold(u.Scheme, "https") && strings.EqualFold(u.Host, host) {
			return true
		}
	}
	return false
}

// setSessionCookie sets the HttpOnly session cookie with Max-Age matching
// the token's remaining lifetime, computed from h.now() rather than wall
// time so it is testable with a mock clock.
func (h *Handler) setSessionCookie(c *gin.Context, token string, expiresAt time.Time, allowedOrigins []string) {
	maxAge := int(expiresAt.Sub(h.now()).Seconds())
	if maxAge < 0 {
		maxAge = 0
	}
	c.SetSameSite(http.SameSiteStrictMode)
	c.SetCookie(SessionCookieName, token, maxAge, sessionCookiePath, "", isHTTPSRequest(c, allowedOrigins), true)
}

// clearSessionCookie removes the session cookie (Max-Age=0).
func (h *Handler) clearSessionCookie(c *gin.Context, allowedOrigins []string) {
	c.SetSameSite(http.SameSiteStrictMode)
	c.SetCookie(SessionCookieName, "", -1, sessionCookiePath, "", isHTTPSRequest(c, allowedOrigins), true)
}

// now returns the handler's current time, via its injectable clock.
func (h *Handler) now() time.Time {
	if h == nil || h.clock == nil {
		return time.Now()
	}
	return h.clock.Now()
}

// decodeLoginSecret base64url-decodes a stored session-secret/user-handle value.
func decodeLoginSecret(value string) ([]byte, error) {
	if value == "" {
		return nil, nil
	}
	return base64.RawURLEncoding.DecodeString(value)
}

// effectivePasskeyOrigins returns the origins WebAuthn ceremonies are
// actually accepted from: the configured list, or ["https://<rp-id>"] when
// that list is empty and an rp-id is set (matching mgmtauth.NewWebAuthn's
// own default), or an empty list when passkeys are not configured at all.
func effectivePasskeyOrigins(account *mgmtauth.Account) []string {
	if account == nil {
		return []string{}
	}
	if len(account.PasskeyOrigins) > 0 {
		return account.PasskeyOrigins
	}
	if account.PasskeyRPID == "" {
		return []string{}
	}
	return []string{"https://" + account.PasskeyRPID}
}

// passkeysAvailable reports whether passkey login is usable: an rp-id is
// configured and at least one passkey registered under that SAME rp-id
// exists. A passkey registered under a since-changed rp-id does not count.
func passkeysAvailable(account *mgmtauth.Account) bool {
	if account == nil || account.PasskeyRPID == "" {
		return false
	}
	for _, p := range account.Passkeys {
		if p.RPID == account.PasskeyRPID {
			return true
		}
	}
	return false
}

// buildWebAuthn constructs a *webauthn.WebAuthn from the account's current
// passkey settings.
func buildWebAuthn(account *mgmtauth.Account) (*webauthn.WebAuthn, error) {
	if account == nil {
		return nil, mgmtauth.ErrPasskeysDisabled
	}
	return mgmtauth.NewWebAuthn(account.PasskeyRPID, sessionRPDisplayName, account.PasskeyOrigins)
}

// mgmtUser decodes the account into a mgmtauth.User usable with the
// WebAuthn ceremonies, including only passkeys registered under the
// account's CURRENT rp-id.
func mgmtUser(account *mgmtauth.Account) (mgmtauth.User, error) {
	if account == nil {
		return mgmtauth.User{}, errors.New("management: no account configured")
	}
	handle, err := decodeLoginSecret(account.UserHandle)
	if err != nil {
		return mgmtauth.User{}, err
	}
	creds := make([]mgmtauth.Credential, 0, len(account.Passkeys))
	for _, p := range account.Passkeys {
		if p.RPID != account.PasskeyRPID {
			continue
		}
		cred, errDecode := decodePasskeyRecord(p)
		if errDecode != nil {
			// A single corrupt record (e.g. hand-edited sidecar file) must
			// not take down every other working passkey; skip and log it.
			log.WithError(errDecode).WithField("passkey_id", p.ID).Error("management: skipping undecodable passkey record")
			continue
		}
		creds = append(creds, cred)
	}
	return mgmtauth.User{Handle: handle, Username: account.Username, Credentials: creds}, nil
}

func decodePasskeyRecord(p mgmtauth.PasskeyRecord) (mgmtauth.Credential, error) {
	id, err := base64.RawURLEncoding.DecodeString(p.ID)
	if err != nil {
		return mgmtauth.Credential{}, err
	}
	pubKey, err := base64.RawURLEncoding.DecodeString(p.PublicKey)
	if err != nil {
		return mgmtauth.Credential{}, err
	}
	var aaguid []byte
	if p.AAGUID != "" {
		aaguid, err = base64.RawURLEncoding.DecodeString(p.AAGUID)
		if err != nil {
			return mgmtauth.Credential{}, err
		}
	}
	return mgmtauth.Credential{
		ID:              id,
		PublicKey:       pubKey,
		AttestationType: p.AttestationType,
		Transports:      p.Transports,
		AAGUID:          aaguid,
		BackupEligible:  p.BackupEligible,
		BackupState:     p.BackupState,
	}, nil
}

func encodePasskeyRecord(c mgmtauth.Credential, rpID, name string, created time.Time) mgmtauth.PasskeyRecord {
	return mgmtauth.PasskeyRecord{
		ID:              base64.RawURLEncoding.EncodeToString(c.ID),
		PublicKey:       base64.RawURLEncoding.EncodeToString(c.PublicKey),
		AttestationType: c.AttestationType,
		Transports:      c.Transports,
		AAGUID:          base64.RawURLEncoding.EncodeToString(c.AAGUID),
		BackupEligible:  c.BackupEligible,
		BackupState:     c.BackupState,
		RPID:            rpID,
		Name:            name,
		Created:         created,
	}
}

// ceilSecondsAtLeastOne rounds d up to a whole number of seconds, never
// returning less than 1: a caller told to retry in "0 seconds" would just
// retry immediately and get throttled again.
func ceilSecondsAtLeastOne(d time.Duration) int {
	secs := int((d + time.Second - 1) / time.Second)
	if secs < 1 {
		secs = 1
	}
	return secs
}

// issueSessionResponse issues a fresh session token, sets the cookie, and
// writes the standard "session response" body described by the management
// login design: {"token": "cpas_...", "expires_at": "<RFC3339>"}. It always
// clears any X-CPA-Session-Refresh header a prior Middleware() step may
// have set (signed with a secret that could be stale, e.g. just before a
// password change rotates it): the body/cookie issued here are always the
// authoritative, freshest credential, so a leftover refresh header must not
// also be sent.
func (h *Handler) issueSessionResponse(c *gin.Context, status int, secret []byte, method mgmtauth.Method, passkeyOrigins []string) {
	token, expiresAt, err := mgmtauth.IssueToken(secret, method, h.now(), mgmtauth.DefaultLifetime)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to issue session: " + err.Error()})
		return
	}
	c.Header("X-CPA-Session-Refresh", "")
	h.setSessionCookie(c, token, expiresAt, passkeyOrigins)
	c.JSON(status, gin.H{"token": token, "expires_at": expiresAt.UTC().Format(time.RFC3339)})
}

// GetSessionStatus reports the caller's authentication state without
// requiring any credential. It recognizes a valid session cookie/bearer
// token or a valid management key, but a missing key is never counted
// against the login-key failure bookkeeping.
func (h *Handler) GetSessionStatus(c *gin.Context) {
	account := h.loginStore.Get()
	origins := effectivePasskeyOrigins(account)

	authenticated := false
	method := ""
	cookieStale := false

	var secret []byte
	if mgmtauth.HasAccount(account) {
		secret, _ = decodeLoginSecret(account.SessionSecret)
	}
	if len(secret) > 0 {
		if token, ok := sessionBearerToken(c); ok {
			if claims, err := mgmtauth.VerifyToken(secret, token, h.now()); err == nil {
				authenticated, method = true, string(claims.Method)
			}
		}
		if !authenticated {
			if token, ok := sessionCookieToken(c); ok {
				if claims, err := mgmtauth.VerifyToken(secret, token, h.now()); err == nil {
					authenticated, method = true, string(claims.Method)
				} else {
					cookieStale = true
				}
			}
		}
	} else if _, ok := sessionCookieToken(c); ok {
		cookieStale = true
	}

	// This never touches the key's failure bookkeeping (B1): a stale cpas_
	// value is excluded by managementKeyFromRequest, and even a genuine
	// wrong-key guess here must not contribute toward banning the caller.
	if !authenticated {
		provided := managementKeyFromRequest(c)
		if provided != "" {
			clientIP := c.ClientIP()
			localClient := clientIP == "127.0.0.1" || clientIP == "::1"
			if h.AuthenticateManagementKeyReadOnly(clientIP, localClient, provided) {
				authenticated = true
				method = string(mgmtauth.MethodKey)
			}
		}
	}

	// A cookie the browser is still holding that no longer verifies is, by
	// definition, no longer any good; clear it here too (not just in
	// Middleware()) so a stale cookie does not keep riding along forever if
	// the caller only ever hits /session/status.
	if cookieStale {
		h.clearSessionCookie(c, origins)
	}

	c.JSON(http.StatusOK, gin.H{
		"account":            mgmtauth.HasAccount(account),
		"authenticated":      authenticated,
		"method":             method,
		"passkeys_available": passkeysAvailable(account),
		"passkey_rp_id":      passkeyRPID(account),
		"passkey_origins":    origins,
	})
}

func passkeyRPID(account *mgmtauth.Account) string {
	if account == nil {
		return ""
	}
	return account.PasskeyRPID
}

// managementKeyFromRequest extracts a management key the same way
// Middleware() does, without consuming it against the failure bookkeeping.
// managementKeyFromRequest extracts a management-key candidate from the
// request: Authorization: Bearer <key> (excluding a cpas_-prefixed value,
// which is a session credential and never a key guess), a raw Authorization
// value, or X-Management-Key.
func managementKeyFromRequest(c *gin.Context) string {
	var provided string
	if ah := c.GetHeader("Authorization"); ah != "" {
		parts := strings.SplitN(ah, " ", 2)
		if len(parts) == 2 && strings.EqualFold(parts[0], "bearer") {
			if !strings.HasPrefix(parts[1], mgmtauth.TokenPrefix) {
				provided = parts[1]
			}
		} else {
			provided = ah
		}
	}
	if provided == "" {
		provided = c.GetHeader("X-Management-Key")
	}
	return provided
}

// PostSessionLogin verifies username/password and, on success, returns a
// session response. It is a public endpoint not covered by Middleware(), so
// it must independently honor the local-or-allow-remote gate key auth uses.
func (h *Handler) PostSessionLogin(c *gin.Context) {
	if !h.remoteAllowed(c) {
		c.JSON(http.StatusForbidden, gin.H{"error": "remote management disabled"})
		return
	}
	limitPublicRequestBody(c)

	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	username := strings.TrimSpace(body.Username)

	account := h.loginStore.Get()
	if !mgmtauth.HasAccount(account) {
		c.JSON(http.StatusConflict, gin.H{"error": "no account configured"})
		return
	}

	release, ok, retryAfter := h.loginThrottle.Reserve()
	if !ok {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "too many attempts", "retry_after": ceilSecondsAtLeastOne(retryAfter)})
		return
	}
	success := false
	func() {
		// Deferred release with a success flag: a panic inside
		// VerifyPassword can never leak the throttle's single slot.
		defer func() { release(success) }()
		match, verifyErr := mgmtauth.VerifyPassword(account.PasswordHash, body.Password)
		success = verifyErr == nil && match && username == account.Username
	}()
	if !success {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
		return
	}

	secret, err := decodeLoginSecret(account.SessionSecret)
	if err != nil || len(secret) == 0 {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "session secret is not configured"})
		return
	}
	h.issueSessionResponse(c, http.StatusOK, secret, mgmtauth.MethodPassword, effectivePasskeyOrigins(account))
}

// PostSessionPasskeyBegin starts a discoverable passkey login ceremony.
func (h *Handler) PostSessionPasskeyBegin(c *gin.Context) {
	if !h.remoteAllowed(c) {
		c.JSON(http.StatusForbidden, gin.H{"error": "remote management disabled"})
		return
	}
	limitPublicRequestBody(c)

	account := h.loginStore.Get()
	if !passkeysAvailable(account) {
		c.JSON(http.StatusConflict, gin.H{"error": "passkeys are not available"})
		return
	}
	w, err := buildWebAuthn(account)
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	assertion, session, err := mgmtauth.BeginPasskeyLogin(w)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	ceremonyID, err := h.loginCeremonies.Begin(*session)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ceremony_id": ceremonyID, "options": assertion})
}

// PostSessionPasskeyFinish completes a discoverable passkey login ceremony.
// Like PostSessionLogin, it is public and must independently honor the
// local-or-allow-remote gate.
func (h *Handler) PostSessionPasskeyFinish(c *gin.Context) {
	if !h.remoteAllowed(c) {
		c.JSON(http.StatusForbidden, gin.H{"error": "remote management disabled"})
		return
	}
	limitPublicRequestBody(c)

	var body struct {
		CeremonyID string          `json:"ceremony_id"`
		Credential json.RawMessage `json:"credential"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}

	session, ok := h.loginCeremonies.Take(body.CeremonyID)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "ceremony expired or already used"})
		return
	}

	account := h.loginStore.Get()
	if !passkeysAvailable(account) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "passkeys are not available"})
		return
	}
	w, err := buildWebAuthn(account)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": err.Error()})
		return
	}
	user, err := mgmtUser(account)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	lookup := func(rawID, userHandle []byte) (webauthn.User, error) {
		if base64.RawURLEncoding.EncodeToString(userHandle) != account.UserHandle {
			return nil, errUnknownUser
		}
		return user, nil
	}

	if _, _, err := mgmtauth.FinishPasskeyLogin(w, lookup, session, body.Credential); err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "passkey login failed"})
		return
	}

	secret, err := decodeLoginSecret(account.SessionSecret)
	if err != nil || len(secret) == 0 {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "session secret is not configured"})
		return
	}
	h.issueSessionResponse(c, http.StatusOK, secret, mgmtauth.MethodPasskey, effectivePasskeyOrigins(account))
}

// PostSessionLogout clears the session cookie.
func (h *Handler) PostSessionLogout(c *gin.Context) {
	limitPublicRequestBody(c)
	account := h.loginStore.Get()
	h.clearSessionCookie(c, effectivePasskeyOrigins(account))
	c.Status(http.StatusNoContent)
}

// maxPublicRequestBodyBytes caps the body size accepted by the public,
// unauthenticated /session/* POST endpoints, so an oversized request body
// cannot be used to exhaust memory before any auth check even runs.
const maxPublicRequestBodyBytes = 64 * 1024

// limitPublicRequestBody wraps the request body in http.MaxBytesReader.
// ShouldBindJSON's decode then fails cleanly once the limit is exceeded,
// rather than reading an unbounded body into memory.
func limitPublicRequestBody(c *gin.Context) {
	if c == nil || c.Request == nil || c.Request.Body == nil {
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxPublicRequestBodyBytes)
}
