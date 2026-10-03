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
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/mgmtauth"
)

// errUnknownUser is returned by the discoverable-login lookup when the
// assertion's user handle does not match the single configured account.
var errUnknownUser = errors.New("management: unknown passkey user")

// SessionCookieName is the HttpOnly cookie carrying a signed session token
// for same-origin panel clients.
const SessionCookieName = "cpa_mgmt_session"

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

// sessionAuthStatus is the outcome of trying to authenticate a request via
// the session cookie/bearer token, before falling back to the management key.
type sessionAuthStatus int

const (
	// sessionAuthNone means no session credential was presented, or the one
	// presented did not verify; the caller should fall through to the
	// management-key check.
	sessionAuthNone sessionAuthStatus = iota
	// sessionAuthOK means the request is authenticated via session.
	sessionAuthOK
	// sessionAuthCSRFBlocked means a valid cookie session was presented but
	// the CSRF guard rejected the request outright.
	sessionAuthCSRFBlocked
)

// tryAuthenticateSession implements the first step of Middleware(): verify a
// cpa_mgmt_session cookie or an Authorization: Bearer cpas_... token, apply
// the CSRF guard for cookie-authenticated unsafe methods, and slide the
// session forward when it is past its halfway point.
func (h *Handler) tryAuthenticateSession(c *gin.Context) sessionAuthStatus {
	if h == nil {
		return sessionAuthNone
	}

	token, viaCookie := sessionTokenFromRequest(c)
	if token == "" {
		return sessionAuthNone
	}

	login := h.currentLoginConfig()
	secret, err := decodeLoginSecret(login.SessionSecret)
	if err != nil || len(secret) == 0 {
		return sessionAuthNone
	}

	now := h.now()
	claims, err := mgmtauth.VerifyToken(secret, token, now)
	if err != nil {
		return sessionAuthNone
	}

	if viaCookie && isUnsafeMethod(c.Request.Method) && !csrfAllowed(c, login.PasskeyOrigins) {
		return sessionAuthCSRFBlocked
	}

	c.Set(LoginMethodContextKey, string(claims.Method))

	if mgmtauth.ShouldRefresh(claims, now, mgmtauth.DefaultLifetime) {
		if refreshed, expiresAt, errIssue := mgmtauth.IssueToken(secret, claims.Method, now, mgmtauth.DefaultLifetime); errIssue == nil {
			if viaCookie {
				setSessionCookie(c, refreshed, expiresAt, login.PasskeyOrigins)
			} else {
				c.Header("X-CPA-Session-Refresh", refreshed)
			}
		}
	}

	return sessionAuthOK
}

// sessionTokenFromRequest extracts a session token from the cookie or an
// Authorization: Bearer cpas_... header. The second return value reports
// whether the token came from the cookie (relevant to the CSRF guard).
func sessionTokenFromRequest(c *gin.Context) (token string, viaCookie bool) {
	if cookie, err := c.Cookie(SessionCookieName); err == nil && strings.HasPrefix(cookie, mgmtauth.TokenPrefix) {
		return cookie, true
	}
	if ah := c.GetHeader("Authorization"); ah != "" {
		parts := strings.SplitN(ah, " ", 2)
		if len(parts) == 2 && strings.EqualFold(parts[0], "bearer") && strings.HasPrefix(parts[1], mgmtauth.TokenPrefix) {
			return parts[1], false
		}
	}
	return "", false
}

func isUnsafeMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	default:
		return true
	}
}

// csrfAllowed implements the CSRF guard for cookie-authenticated unsafe
// requests: Sec-Fetch-Site: same-origin is trusted if present; otherwise the
// Origin host must equal the request Host, or Origin must be one of the
// configured passkey origins. A request with neither header is rejected.
func csrfAllowed(c *gin.Context, passkeyOrigins []string) bool {
	if sfs := c.GetHeader("Sec-Fetch-Site"); sfs != "" {
		return sfs == "same-origin"
	}
	origin := strings.TrimSpace(c.GetHeader("Origin"))
	if origin == "" {
		return false
	}
	if u, err := url.Parse(origin); err == nil && strings.EqualFold(u.Host, c.Request.Host) {
		return true
	}
	trimmed := strings.TrimRight(origin, "/")
	for _, allowed := range passkeyOrigins {
		if strings.EqualFold(strings.TrimRight(strings.TrimSpace(allowed), "/"), trimmed) {
			return true
		}
	}
	return false
}

// isHTTPSRequest reports whether the current request should be treated as
// HTTPS for the purposes of the session cookie's Secure attribute: direct
// TLS, X-Forwarded-Proto: https, or an https Origin header (the case that
// matters when a TLS-terminating proxy forwards X-Forwarded-Proto: http).
func isHTTPSRequest(c *gin.Context) bool {
	if c.Request.TLS != nil {
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
	return false
}

// setSessionCookie sets the HttpOnly session cookie with Max-Age matching
// the token's remaining lifetime.
func setSessionCookie(c *gin.Context, token string, expiresAt time.Time, passkeyOrigins []string) {
	maxAge := int(time.Until(expiresAt).Seconds())
	if maxAge < 0 {
		maxAge = 0
	}
	c.SetSameSite(http.SameSiteStrictMode)
	c.SetCookie(SessionCookieName, token, maxAge, "/", "", isHTTPSRequest(c), true)
}

// clearSessionCookie removes the session cookie (Max-Age=0).
func clearSessionCookie(c *gin.Context) {
	c.SetSameSite(http.SameSiteStrictMode)
	c.SetCookie(SessionCookieName, "", -1, "/", "", isHTTPSRequest(c), true)
}

// now returns the handler's current time, via its injectable clock.
func (h *Handler) now() time.Time {
	if h == nil || h.clock == nil {
		return time.Now()
	}
	return h.clock.Now()
}

// currentLoginConfig returns a snapshot of the current login account config.
func (h *Handler) currentLoginConfig() config.LoginConfig {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cfg == nil {
		return config.LoginConfig{}
	}
	return h.cfg.RemoteManagement.Login
}

// decodeLoginSecret base64url-decodes a stored session-secret/user-handle value.
func decodeLoginSecret(value string) ([]byte, error) {
	if value == "" {
		return nil, nil
	}
	return base64.RawURLEncoding.DecodeString(value)
}

// hasAccount reports whether a username/password account is configured.
func hasAccount(login config.LoginConfig) bool {
	return login.Username != "" && login.PasswordHash != ""
}

// passkeysAvailable reports whether passkey login is usable: an rp-id is
// configured and at least one passkey is registered.
func passkeysAvailable(login config.LoginConfig) bool {
	return login.PasskeyRPID != "" && len(login.Passkeys) > 0
}

// buildWebAuthn constructs a *webauthn.WebAuthn from the current login
// config's passkey settings.
func buildWebAuthn(login config.LoginConfig) (*webauthn.WebAuthn, error) {
	return mgmtauth.NewWebAuthn(login.PasskeyRPID, sessionRPDisplayName, login.PasskeyOrigins)
}

// mgmtUser decodes the stored account into a mgmtauth.User usable with the
// WebAuthn ceremonies.
func mgmtUser(login config.LoginConfig) (mgmtauth.User, error) {
	handle, err := decodeLoginSecret(login.UserHandle)
	if err != nil {
		return mgmtauth.User{}, err
	}
	creds := make([]mgmtauth.Credential, 0, len(login.Passkeys))
	for _, p := range login.Passkeys {
		cred, errDecode := decodePasskeyCredential(p)
		if errDecode != nil {
			return mgmtauth.User{}, errDecode
		}
		creds = append(creds, cred)
	}
	return mgmtauth.User{Handle: handle, Username: login.Username, Credentials: creds}, nil
}

func decodePasskeyCredential(p config.PasskeyCredential) (mgmtauth.Credential, error) {
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

func encodePasskeyCredential(c mgmtauth.Credential, name string, created time.Time) config.PasskeyCredential {
	return config.PasskeyCredential{
		ID:              base64.RawURLEncoding.EncodeToString(c.ID),
		PublicKey:       base64.RawURLEncoding.EncodeToString(c.PublicKey),
		AttestationType: c.AttestationType,
		Transports:      c.Transports,
		AAGUID:          base64.RawURLEncoding.EncodeToString(c.AAGUID),
		BackupEligible:  c.BackupEligible,
		BackupState:     c.BackupState,
		Name:            name,
		Created:         created,
	}
}

// issueSessionResponse issues a fresh session token, sets the cookie, and
// writes the standard "session response" body described by the management
// login design: {"token": "cpas_...", "expires_at": "<RFC3339>"}.
func (h *Handler) issueSessionResponse(c *gin.Context, status int, secret []byte, method mgmtauth.Method, passkeyOrigins []string) {
	token, expiresAt, err := mgmtauth.IssueToken(secret, method, h.now(), mgmtauth.DefaultLifetime)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to issue session: " + err.Error()})
		return
	}
	setSessionCookie(c, token, expiresAt, passkeyOrigins)
	c.JSON(status, gin.H{"token": token, "expires_at": expiresAt.UTC().Format(time.RFC3339)})
}

// GetSessionStatus reports the caller's authentication state without
// requiring any credential. It recognizes a valid session cookie/bearer
// token or a valid management key, but a missing key is never counted
// against the login-key failure bookkeeping.
func (h *Handler) GetSessionStatus(c *gin.Context) {
	login := h.currentLoginConfig()

	authenticated := false
	method := ""

	if token, _ := sessionTokenFromRequest(c); token != "" {
		if secret, err := decodeLoginSecret(login.SessionSecret); err == nil && len(secret) > 0 {
			if claims, errVerify := mgmtauth.VerifyToken(secret, token, h.now()); errVerify == nil {
				authenticated = true
				method = string(claims.Method)
			}
		}
	}

	if !authenticated {
		provided := managementKeyFromRequest(c)
		if provided != "" {
			clientIP := c.ClientIP()
			localClient := clientIP == "127.0.0.1" || clientIP == "::1"
			if ok, _, _ := h.AuthenticateManagementKey(clientIP, localClient, provided); ok {
				authenticated = true
				method = string(mgmtauth.MethodKey)
			}
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"account":            hasAccount(login),
		"authenticated":      authenticated,
		"method":             method,
		"passkeys_available": passkeysAvailable(login),
		"passkey_rp_id":      login.PasskeyRPID,
		"passkey_origins":    effectivePasskeyOrigins(login),
	})
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

// managementKeyFromRequest extracts a management key the same way
// Middleware() does, without consuming it against the failure bookkeeping.
func managementKeyFromRequest(c *gin.Context) string {
	var provided string
	if ah := c.GetHeader("Authorization"); ah != "" {
		parts := strings.SplitN(ah, " ", 2)
		if len(parts) == 2 && strings.EqualFold(parts[0], "bearer") {
			provided = parts[1]
		} else {
			provided = ah
		}
	}
	if provided == "" {
		provided = c.GetHeader("X-Management-Key")
	}
	return provided
}

// effectivePasskeyOrigins returns the origins WebAuthn ceremonies are
// actually accepted from: the configured list, or ["https://<rp-id>"] when
// that list is empty and an rp-id is set (matching mgmtauth.NewWebAuthn's
// own default), or an empty list when passkeys are not configured at all.
func effectivePasskeyOrigins(login config.LoginConfig) []string {
	if len(login.PasskeyOrigins) > 0 {
		return login.PasskeyOrigins
	}
	if login.PasskeyRPID == "" {
		return []string{}
	}
	return []string{"https://" + login.PasskeyRPID}
}

// PostSessionLogin verifies username/password and, on success, returns a
// session response.
func (h *Handler) PostSessionLogin(c *gin.Context) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}

	login := h.currentLoginConfig()
	if !hasAccount(login) {
		c.JSON(http.StatusConflict, gin.H{"error": "no account configured"})
		return
	}

	if ok, retryAfter := h.loginThrottle.Allow(); !ok {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "too many attempts", "retry_after": ceilSecondsAtLeastOne(retryAfter)})
		return
	}

	match, err := mgmtauth.VerifyPassword(login.PasswordHash, body.Password)
	if err != nil || !match || body.Username != login.Username {
		h.loginThrottle.RecordFailure()
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
		return
	}
	h.loginThrottle.Reset()

	secret, err := decodeLoginSecret(login.SessionSecret)
	if err != nil || len(secret) == 0 {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "session secret is not configured"})
		return
	}
	h.issueSessionResponse(c, http.StatusOK, secret, mgmtauth.MethodPassword, login.PasskeyOrigins)
}

// PostSessionPasskeyBegin starts a discoverable passkey login ceremony.
func (h *Handler) PostSessionPasskeyBegin(c *gin.Context) {
	login := h.currentLoginConfig()
	if !passkeysAvailable(login) {
		c.JSON(http.StatusConflict, gin.H{"error": "passkeys are not available"})
		return
	}
	w, err := buildWebAuthn(login)
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	assertion, session, err := mgmtauth.BeginPasskeyLogin(w)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	ceremonyID, err := h.passkeyCeremonies.Begin(*session)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ceremony_id": ceremonyID, "options": assertion})
}

// PostSessionPasskeyFinish completes a discoverable passkey login ceremony.
func (h *Handler) PostSessionPasskeyFinish(c *gin.Context) {
	var body struct {
		CeremonyID string          `json:"ceremony_id"`
		Credential json.RawMessage `json:"credential"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}

	session, ok := h.passkeyCeremonies.Take(body.CeremonyID)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "ceremony expired or already used"})
		return
	}

	login := h.currentLoginConfig()
	if !passkeysAvailable(login) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "passkeys are not available"})
		return
	}
	w, err := buildWebAuthn(login)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": err.Error()})
		return
	}
	user, err := mgmtUser(login)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	lookup := func(rawID, userHandle []byte) (webauthn.User, error) {
		if base64.RawURLEncoding.EncodeToString(userHandle) != login.UserHandle {
			return nil, errUnknownUser
		}
		return user, nil
	}

	if _, _, err := mgmtauth.FinishPasskeyLogin(w, lookup, session, body.Credential); err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "passkey login failed"})
		return
	}

	secret, err := decodeLoginSecret(login.SessionSecret)
	if err != nil || len(secret) == 0 {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "session secret is not configured"})
		return
	}
	h.issueSessionResponse(c, http.StatusOK, secret, mgmtauth.MethodPasskey, login.PasskeyOrigins)
}

// PostSessionLogout clears the session cookie.
func (h *Handler) PostSessionLogout(c *gin.Context) {
	clearSessionCookie(c)
	c.Status(http.StatusNoContent)
}
