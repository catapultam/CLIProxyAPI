package management

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/mgmtauth"
)

// minPasswordLength is the policy floor enforced by PUT /account.
const minPasswordLength = 12

// applyLoginMutation holds h.mu for the full validate+mutate+save sequence
// used by every account-mutating endpoint: mutate runs under the lock and
// may reject the request by returning a non-zero status and message. On
// success the config is saved and the post-save reload hook is scheduled
// asynchronously, matching the pattern other management handlers use via
// saveConfigAndSnapshotLocked/reloadConfigAfterManagementSaveAsync.
func (h *Handler) applyLoginMutation(c *gin.Context, mutate func(login *config.LoginConfig) (status int, errMsg string)) (config.LoginConfig, bool) {
	h.mu.Lock()
	if h.cfg == nil {
		h.mu.Unlock()
		c.JSON(http.StatusInternalServerError, gin.H{"error": "config unavailable"})
		return config.LoginConfig{}, false
	}
	if status, errMsg := mutate(&h.cfg.RemoteManagement.Login); errMsg != "" {
		h.mu.Unlock()
		c.JSON(status, gin.H{"error": errMsg})
		return config.LoginConfig{}, false
	}
	snapshot, ok := h.saveConfigAndSnapshotLocked(c)
	login := h.cfg.RemoteManagement.Login
	h.mu.Unlock()
	if !ok {
		return config.LoginConfig{}, false
	}
	var reqCtx context.Context
	if c != nil && c.Request != nil {
		reqCtx = c.Request.Context()
	}
	h.reloadConfigAfterManagementSaveAsync(reqCtx, snapshot)
	return login, true
}

// generateAccountSecret returns 32 random bytes, base64url-encoded, used for
// both session-secret and user-handle.
func generateAccountSecret() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// accountView builds the GET /account response body.
func accountView(login config.LoginConfig) gin.H {
	passkeys := make([]gin.H, 0, len(login.Passkeys))
	for _, p := range login.Passkeys {
		passkeys = append(passkeys, passkeyView(p))
	}
	return gin.H{
		"configured":      hasAccount(login),
		"username":        login.Username,
		"passkeys":        passkeys,
		"passkey_rp_id":   login.PasskeyRPID,
		"passkey_origins": effectivePasskeyOrigins(login),
	}
}

func passkeyView(p config.PasskeyCredential) gin.H {
	return gin.H{"id": p.ID, "name": p.Name, "created_at": p.Created.UTC().Format(time.RFC3339)}
}

// GetAccount returns the current account configuration.
func (h *Handler) GetAccount(c *gin.Context) {
	c.JSON(http.StatusOK, accountView(h.currentLoginConfig()))
}

// PutAccount creates or updates the single management-login account.
//
// On first setup, password is required (minimum 12 characters). For an
// existing account, an empty password means "keep the current password", so
// a username-only change is valid; a non-empty password must still meet the
// minimum length. Over a session, current_password is required for ANY
// change to an existing account (username or password); over the management
// key it is never required. session-secret is rotated only when the
// password actually changes, but the caller always gets a fresh session
// response regardless.
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

	authViaSession := c.GetString(AuthMethodContextKey) == AuthMethodSession

	login, ok := h.applyLoginMutation(c, func(l *config.LoginConfig) (int, string) {
		firstSetup := !hasAccount(*l)
		if firstSetup && body.Password == "" {
			return http.StatusBadRequest, "password is required"
		}
		if !firstSetup && authViaSession {
			if body.CurrentPassword == "" {
				return http.StatusForbidden, "current_password is required"
			}
			match, errVerify := mgmtauth.VerifyPassword(l.PasswordHash, body.CurrentPassword)
			if errVerify != nil || !match {
				return http.StatusForbidden, "current_password is incorrect"
			}
		}

		l.Username = username

		passwordChanged := false
		if body.Password != "" {
			hashed, errHash := mgmtauth.HashPassword(body.Password)
			if errHash != nil {
				return http.StatusInternalServerError, errHash.Error()
			}
			l.PasswordHash = hashed
			l.Password = ""
			passwordChanged = true
		}

		if firstSetup {
			if l.SessionSecret == "" {
				secret, errGen := generateAccountSecret()
				if errGen != nil {
					return http.StatusInternalServerError, errGen.Error()
				}
				l.SessionSecret = secret
			}
			if l.UserHandle == "" {
				handle, errGen := generateAccountSecret()
				if errGen != nil {
					return http.StatusInternalServerError, errGen.Error()
				}
				l.UserHandle = handle
			}
		} else if passwordChanged {
			// A password change invalidates every existing session.
			secret, errGen := generateAccountSecret()
			if errGen != nil {
				return http.StatusInternalServerError, errGen.Error()
			}
			l.SessionSecret = secret
		}
		return 0, ""
	})
	if !ok {
		return
	}

	secret, err := decodeLoginSecret(login.SessionSecret)
	if err != nil || len(secret) == 0 {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "session secret is not configured"})
		return
	}
	h.issueSessionResponse(c, http.StatusOK, secret, mgmtauth.MethodPassword, login.PasskeyOrigins)
}

// PutAccountPasskeySettings updates the WebAuthn relying party id/origins.
func (h *Handler) PutAccountPasskeySettings(c *gin.Context) {
	var body struct {
		RPID    string   `json:"rp_id"`
		Origins []string `json:"origins"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	login, ok := h.applyLoginMutation(c, func(l *config.LoginConfig) (int, string) {
		l.PasskeyRPID = strings.TrimSpace(body.RPID)
		l.PasskeyOrigins = body.Origins
		return 0, ""
	})
	if !ok {
		return
	}
	c.JSON(http.StatusOK, accountView(login))
}

// PostAccountPasskeysBegin starts a ceremony to register a new passkey.
func (h *Handler) PostAccountPasskeysBegin(c *gin.Context) {
	login := h.currentLoginConfig()
	if login.PasskeyRPID == "" || !hasAccount(login) {
		c.JSON(http.StatusConflict, gin.H{"error": "passkeys are not configured"})
		return
	}
	w, err := buildWebAuthn(login)
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	user, err := mgmtUser(login)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	creation, session, err := mgmtauth.BeginAddPasskey(w, user)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	ceremonyID, err := h.passkeyCeremonies.Begin(*session)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ceremony_id": ceremonyID, "options": creation})
}

// PostAccountPasskeysFinish completes a passkey registration ceremony and
// persists the new credential.
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

	session, ok := h.passkeyCeremonies.Take(body.CeremonyID)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "ceremony expired or already used"})
		return
	}

	login := h.currentLoginConfig()
	w, err := buildWebAuthn(login)
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	user, err := mgmtUser(login)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	cred, err := mgmtauth.FinishAddPasskey(w, user, session, body.Credential)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "passkey registration failed"})
		return
	}
	stored := encodePasskeyCredential(mgmtauth.FromWebAuthnCredential(cred), strings.TrimSpace(body.Name), h.now())

	if _, ok := h.applyLoginMutation(c, func(l *config.LoginConfig) (int, string) {
		l.Passkeys = append(l.Passkeys, stored)
		return 0, ""
	}); !ok {
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

	var updated config.PasskeyCredential
	if _, ok := h.applyLoginMutation(c, func(l *config.LoginConfig) (int, string) {
		for i := range l.Passkeys {
			if l.Passkeys[i].ID == id {
				l.Passkeys[i].Name = body.Name
				updated = l.Passkeys[i]
				return 0, ""
			}
		}
		return http.StatusNotFound, "passkey not found"
	}); !ok {
		return
	}
	c.JSON(http.StatusOK, passkeyView(updated))
}

// DeleteAccountPasskey removes a registered passkey.
func (h *Handler) DeleteAccountPasskey(c *gin.Context) {
	id := c.Param("id")
	if _, ok := h.applyLoginMutation(c, func(l *config.LoginConfig) (int, string) {
		out := make([]config.PasskeyCredential, 0, len(l.Passkeys))
		found := false
		for _, p := range l.Passkeys {
			if p.ID == id {
				found = true
				continue
			}
			out = append(out, p)
		}
		if !found {
			return http.StatusNotFound, "passkey not found"
		}
		l.Passkeys = out
		return 0, ""
	}); !ok {
		return
	}
	c.Status(http.StatusNoContent)
}

// PostAccountSignOutAll rotates session-secret, invalidating every existing
// session, and clears the caller's own cookie.
func (h *Handler) PostAccountSignOutAll(c *gin.Context) {
	if _, ok := h.applyLoginMutation(c, func(l *config.LoginConfig) (int, string) {
		secret, err := generateAccountSecret()
		if err != nil {
			return http.StatusInternalServerError, err.Error()
		}
		l.SessionSecret = secret
		return 0, ""
	}); !ok {
		return
	}
	clearSessionCookie(c)
	c.Status(http.StatusNoContent)
}
