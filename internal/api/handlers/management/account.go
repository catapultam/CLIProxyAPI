package management

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/mgmtauth"
)

// generateAccountSecret returns 32 random bytes, base64url-encoded, used
// for both session-secret and user-handle.
func generateAccountSecret() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// minPasswordLength is the policy floor enforced by PUT /account.
const minPasswordLength = 12

// accountView builds the GET /account response body.
func accountView(account *mgmtauth.Account) gin.H {
	var passkeys []gin.H
	if account != nil {
		passkeys = make([]gin.H, 0, len(account.Passkeys))
		for _, p := range account.Passkeys {
			passkeys = append(passkeys, passkeyView(p))
		}
	} else {
		passkeys = []gin.H{}
	}
	username := ""
	if account != nil {
		username = account.Username
	}
	return gin.H{
		"configured":      mgmtauth.HasAccount(account),
		"username":        username,
		"passkeys":        passkeys,
		"passkey_rp_id":   passkeyRPID(account),
		"passkey_origins": effectivePasskeyOrigins(account),
	}
}

func passkeyView(p mgmtauth.PasskeyRecord) gin.H {
	return gin.H{"id": p.ID, "name": p.Name, "created_at": p.Created.UTC().Format(time.RFC3339)}
}

// mutationErrorResponse writes the appropriate status/body for a
// Store.Mutate error: a *mgmtauth.MutationError carries its own status
// (e.g. 404 "not found", 409 "changed concurrently", 503 "store
// unavailable"), anything else is an unexpected internal failure (500).
func mutationErrorResponse(c *gin.Context, err error) {
	var mutErr *mgmtauth.MutationError
	if errors.As(err, &mutErr) {
		c.JSON(mutErr.Status, gin.H{"error": mutErr.Message})
		return
	}
	c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
}

// resolvedSessionMethod picks the method label a freshly issued session
// should carry: a passkey-authenticated session stays passkey, anything
// else (a password session, or a management key) defaults to password,
// since a token can only ever carry password or passkey.
func resolvedSessionMethod(c *gin.Context) mgmtauth.Method {
	if c.GetString(LoginMethodContextKey) == string(mgmtauth.MethodPasskey) {
		return mgmtauth.MethodPasskey
	}
	return mgmtauth.MethodPassword
}

// bindOptionalJSON decodes body's JSON into dst, tolerating a completely
// empty request body (treated as leaving dst at its zero value): callers
// authenticated via the management key legitimately send no body at all to
// endpoints that only need current_password over a session.
func bindOptionalJSON(c *gin.Context, dst any) bool {
	if err := c.ShouldBindJSON(dst); err != nil && !errors.Is(err, io.EOF) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return false
	}
	return true
}

// verifyCurrentPassword checks currentPassword against expectedHash through
// the shared login throttle's single-flight admission, writing the
// appropriate error response and returning false on any failure (missing
// value, throttled, or wrong). The release is deferred with a success flag
// set just before it, so a panic between Reserve and the deferred call can
// never leak the throttle's slot.
func (h *Handler) verifyCurrentPassword(c *gin.Context, expectedHash, currentPassword string) bool {
	if currentPassword == "" {
		c.JSON(http.StatusForbidden, gin.H{"error": "current_password is required"})
		return false
	}
	release, ok, retryAfter := h.loginThrottle.Reserve()
	if !ok {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "too many attempts", "retry_after": ceilSecondsAtLeastOne(retryAfter)})
		return false
	}
	success := false
	defer func() { release(success) }()

	match, verifyErr := mgmtauth.VerifyPassword(expectedHash, currentPassword)
	success = verifyErr == nil && match
	if !success {
		c.JSON(http.StatusForbidden, gin.H{"error": "current_password is incorrect"})
		return false
	}
	return true
}

// GetAccount returns the current account configuration.
func (h *Handler) GetAccount(c *gin.Context) {
	c.JSON(http.StatusOK, accountView(h.loginStore.Get()))
}

// PutAccount creates or updates the single management-login account.
//
// On first setup, password is required (minimum 12 characters) and a fresh
// session-secret/user-handle are always generated. For an existing account,
// an empty password means "keep the current password", so a username-only
// change is valid; a non-empty password must still meet the minimum
// length. Over a session, current_password is required for ANY change to
// an existing account (username or password); over the management key it is
// never required. session-secret is rotated only when the password
// actually changes, but the caller always gets a fresh session response
// regardless, carrying its own authentication method forward.
func (h *Handler) PutAccount(c *gin.Context) {
	var body struct {
		Username        string `json:"username"`
		Password        string `json:"password"`
		CurrentPassword string `json:"current_password"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	username := strings.TrimSpace(body.Username)
	if username == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "username must be non-empty"})
		return
	}
	if body.Password != "" && len(body.Password) < minPasswordLength {
		c.JSON(http.StatusBadRequest, gin.H{"error": "password must be at least 12 characters"})
		return
	}

	current := h.loginStore.Get()
	firstSetup := !mgmtauth.HasAccount(current)
	if firstSetup && body.Password == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "password is required"})
		return
	}

	expectedCurrentHash := ""
	if current != nil {
		expectedCurrentHash = current.PasswordHash
	}
	authViaSession := c.GetString(AuthMethodContextKey) == AuthMethodSession
	if !firstSetup && authViaSession {
		// current_password verification goes through the same single-flight
		// throttle as login, since a held session/key is not proof of the
		// current password and must not let an attacker brute-force it.
		if !h.verifyCurrentPassword(c, expectedCurrentHash, body.CurrentPassword) {
			return
		}
	}

	// N1: the new password is hashed here, before Mutate, so the slow
	// argon2 call never runs while Store.mu is held.
	passwordChanging := body.Password != ""
	var newHash string
	if passwordChanging {
		hashed, errHash := mgmtauth.HashPassword(body.Password)
		if errHash != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": errHash.Error()})
			return
		}
		newHash = hashed
	}

	next, err := h.loginStore.Mutate(func(acct *mgmtauth.Account) (*mgmtauth.Account, error) {
		out := acct
		stillFirstSetup := !mgmtauth.HasAccount(out)
		if !stillFirstSetup && authViaSession {
			// N2: current_password was checked against a snapshot taken
			// before this Mutate call; re-check it is still the current
			// value, so a concurrent password change cannot be silently
			// overwritten by a request authorized against a stale hash.
			if out.PasswordHash != expectedCurrentHash {
				return nil, &mgmtauth.MutationError{Status: http.StatusConflict, Message: "account changed concurrently; retry"}
			}
		}
		if out == nil {
			out = &mgmtauth.Account{}
		}
		out.Username = username
		if passwordChanging {
			out.PasswordHash = newHash
		}

		if stillFirstSetup {
			secret, errGen := generateAccountSecret()
			if errGen != nil {
				return nil, errGen
			}
			out.SessionSecret = secret
			handle, errGen2 := generateAccountSecret()
			if errGen2 != nil {
				return nil, errGen2
			}
			out.UserHandle = handle
		} else if passwordChanging {
			// A password change invalidates every existing session.
			secret, errGen := generateAccountSecret()
			if errGen != nil {
				return nil, errGen
			}
			out.SessionSecret = secret
		}
		return out, nil
	})
	if err != nil {
		mutationErrorResponse(c, err)
		return
	}

	secret, errDecode := decodeLoginSecret(next.SessionSecret)
	if errDecode != nil || len(secret) == 0 {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "session secret is not configured"})
		return
	}
	h.issueSessionResponse(c, http.StatusOK, secret, resolvedSessionMethod(c), effectivePasskeyOrigins(next))
}

// PutAccountPasskeySettings updates the WebAuthn relying party id/origins.
// Over a session, current_password is required (S7); over the key it is
// not.
func (h *Handler) PutAccountPasskeySettings(c *gin.Context) {
	var body struct {
		RPID            string   `json:"rp_id"`
		Origins         []string `json:"origins"`
		CurrentPassword string   `json:"current_password"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}

	if c.GetString(AuthMethodContextKey) == AuthMethodSession {
		current := h.loginStore.Get()
		expectedHash := ""
		if current != nil {
			expectedHash = current.PasswordHash
		}
		if !h.verifyCurrentPassword(c, expectedHash, body.CurrentPassword) {
			return
		}
	}

	next, err := h.loginStore.Mutate(func(acct *mgmtauth.Account) (*mgmtauth.Account, error) {
		out := acct
		if out == nil {
			out = &mgmtauth.Account{}
		}
		out.PasskeyRPID = strings.TrimSpace(body.RPID)
		out.PasskeyOrigins = body.Origins
		return out, nil
	})
	if err != nil {
		mutationErrorResponse(c, err)
		return
	}
	c.JSON(http.StatusOK, accountView(next))
}

// PostAccountPasskeysBegin starts a ceremony to register a new passkey.
// Over a session, current_password is required (S7, body {"current_password"});
// over the key it is not, and the body may be omitted entirely.
func (h *Handler) PostAccountPasskeysBegin(c *gin.Context) {
	var body struct {
		CurrentPassword string `json:"current_password"`
	}
	if !bindOptionalJSON(c, &body) {
		return
	}

	account := h.loginStore.Get()
	if account == nil || account.PasskeyRPID == "" || !mgmtauth.HasAccount(account) {
		c.JSON(http.StatusConflict, gin.H{"error": "passkeys are not configured"})
		return
	}
	if c.GetString(AuthMethodContextKey) == AuthMethodSession {
		if !h.verifyCurrentPassword(c, account.PasswordHash, body.CurrentPassword) {
			return
		}
	}

	w, err := buildWebAuthn(account)
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	user, err := mgmtUser(account)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	creation, session, err := mgmtauth.BeginAddPasskey(w, user)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	ceremonyID, err := h.registrationCeremonies.Begin(*session)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ceremony_id": ceremonyID, "options": creation})
}

// PostAccountPasskeysFinish completes a passkey registration ceremony and
// persists the new credential, tagged with the account's current rp-id.
//
// This is an authenticated route: per the panel's "every 401 means logged
// out" convention, ceremony/verification failures here use 410 and 400
// rather than 401, which stays reserved for "not authenticated".
func (h *Handler) PostAccountPasskeysFinish(c *gin.Context) {
	var body struct {
		CeremonyID string          `json:"ceremony_id"`
		Name       string          `json:"name"`
		Credential json.RawMessage `json:"credential"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}

	session, ok := h.registrationCeremonies.Take(body.CeremonyID)
	if !ok {
		c.JSON(http.StatusGone, gin.H{"error": "ceremony expired or already used"})
		return
	}

	account := h.loginStore.Get()
	w, err := buildWebAuthn(account)
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	user, err := mgmtUser(account)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	cred, err := mgmtauth.FinishAddPasskey(w, user, session, body.Credential)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "passkey registration failed"})
		return
	}
	rpID := account.PasskeyRPID
	stored := encodePasskeyRecord(mgmtauth.FromWebAuthnCredential(cred), rpID, strings.TrimSpace(body.Name), h.now())

	if _, err := h.loginStore.Mutate(func(acct *mgmtauth.Account) (*mgmtauth.Account, error) {
		if acct == nil {
			return nil, &mgmtauth.MutationError{Status: http.StatusConflict, Message: "no account configured"}
		}
		for _, existing := range acct.Passkeys {
			if existing.ID == stored.ID {
				// N11: a duplicate credential ID (the same physical key
				// registered twice) must be rejected, not silently
				// duplicated in the store.
				return nil, &mgmtauth.MutationError{Status: http.StatusConflict, Message: "passkey already registered"}
			}
		}
		acct.Passkeys = append(acct.Passkeys, stored)
		return acct, nil
	}); err != nil {
		mutationErrorResponse(c, err)
		return
	}
	c.JSON(http.StatusOK, passkeyView(stored))
}

// PatchAccountPasskey renames a registered passkey.
func (h *Handler) PatchAccountPasskey(c *gin.Context) {
	id := c.Param("id")
	var body struct {
		Name string `json:"name"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}

	var updated mgmtauth.PasskeyRecord
	_, err := h.loginStore.Mutate(func(acct *mgmtauth.Account) (*mgmtauth.Account, error) {
		if acct == nil {
			return nil, &mgmtauth.MutationError{Status: http.StatusNotFound, Message: "passkey not found"}
		}
		for i := range acct.Passkeys {
			if acct.Passkeys[i].ID == id {
				acct.Passkeys[i].Name = body.Name
				updated = acct.Passkeys[i]
				return acct, nil
			}
		}
		return nil, &mgmtauth.MutationError{Status: http.StatusNotFound, Message: "passkey not found"}
	})
	if err != nil {
		mutationErrorResponse(c, err)
		return
	}
	c.JSON(http.StatusOK, passkeyView(updated))
}

// DeleteAccountPasskey removes a registered passkey.
func (h *Handler) DeleteAccountPasskey(c *gin.Context) {
	id := c.Param("id")
	_, err := h.loginStore.Mutate(func(acct *mgmtauth.Account) (*mgmtauth.Account, error) {
		if acct == nil {
			return nil, &mgmtauth.MutationError{Status: http.StatusNotFound, Message: "passkey not found"}
		}
		out := make([]mgmtauth.PasskeyRecord, 0, len(acct.Passkeys))
		found := false
		for _, p := range acct.Passkeys {
			if p.ID == id {
				found = true
				continue
			}
			out = append(out, p)
		}
		if !found {
			return nil, &mgmtauth.MutationError{Status: http.StatusNotFound, Message: "passkey not found"}
		}
		acct.Passkeys = out
		return acct, nil
	})
	if err != nil {
		mutationErrorResponse(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// PostAccountSignOutAll rotates session-secret, invalidating every existing
// session, and clears the caller's own cookie.
func (h *Handler) PostAccountSignOutAll(c *gin.Context) {
	next, err := h.loginStore.Mutate(func(acct *mgmtauth.Account) (*mgmtauth.Account, error) {
		if acct == nil {
			return nil, &mgmtauth.MutationError{Status: http.StatusConflict, Message: "no account configured"}
		}
		secret, errGen := generateAccountSecret()
		if errGen != nil {
			return nil, errGen
		}
		acct.SessionSecret = secret
		return acct, nil
	})
	if err != nil {
		mutationErrorResponse(c, err)
		return
	}
	// N4: this issues no new token, so any refresh header a prior
	// Middleware() step attached (signed with the secret just rotated away)
	// must not ride along either.
	c.Header("X-CPA-Session-Refresh", "")
	h.clearSessionCookie(c, effectivePasskeyOrigins(next))
	c.Status(http.StatusNoContent)
}
