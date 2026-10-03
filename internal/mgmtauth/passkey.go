package mgmtauth

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
)

// CeremonyTTL is how long a passkey registration/login ceremony challenge
// stays valid. A restart in the middle of a ceremony just means pressing the
// button again.
const CeremonyTTL = 5 * time.Minute

// ErrPasskeysDisabled is returned when passkey-rp-id is not configured.
var ErrPasskeysDisabled = errors.New("mgmtauth: passkeys are not configured")

// Credential is the subset of webauthn.Credential persisted in config.yaml
// under management.login.passkeys. Sign counters are intentionally not
// stored: synced passkeys always report 0, and persisting the counter would
// rewrite config.yaml on every login.
type Credential struct {
	ID              []byte
	PublicKey       []byte
	AttestationType string
	Transports      []string
	AAGUID          []byte
	BackupEligible  bool
	BackupState     bool
}

// User adapts the single management-login account into a webauthn.User.
type User struct {
	Handle      []byte
	Username    string
	Credentials []Credential
}

// WebAuthnID returns the WebAuthn user handle.
func (u User) WebAuthnID() []byte { return u.Handle }

// WebAuthnName returns the account username.
func (u User) WebAuthnName() string { return u.Username }

// WebAuthnDisplayName returns the account username as the display name; the
// single-account design has no separate human-friendly display name.
func (u User) WebAuthnDisplayName() string { return u.Username }

// WebAuthnCredentials returns the account's registered passkeys converted to
// webauthn.Credential.
func (u User) WebAuthnCredentials() []webauthn.Credential {
	out := make([]webauthn.Credential, 0, len(u.Credentials))
	for _, c := range u.Credentials {
		out = append(out, toWebAuthnCredential(c))
	}
	return out
}

func toWebAuthnCredential(c Credential) webauthn.Credential {
	transports := make([]protocol.AuthenticatorTransport, 0, len(c.Transports))
	for _, t := range c.Transports {
		transports = append(transports, protocol.AuthenticatorTransport(t))
	}
	return webauthn.Credential{
		ID:              c.ID,
		PublicKey:       c.PublicKey,
		AttestationType: c.AttestationType,
		Transport:       transports,
		Flags: webauthn.CredentialFlags{
			BackupEligible: c.BackupEligible,
			BackupState:    c.BackupState,
		},
		Authenticator: webauthn.Authenticator{
			AAGUID: c.AAGUID,
		},
	}
}

// FromWebAuthnCredential converts a verified webauthn.Credential (the result
// of a successful registration) into the persisted Credential shape.
func FromWebAuthnCredential(c *webauthn.Credential) Credential {
	transports := make([]string, 0, len(c.Transport))
	for _, t := range c.Transport {
		transports = append(transports, string(t))
	}
	return Credential{
		ID:              append([]byte(nil), c.ID...),
		PublicKey:       append([]byte(nil), c.PublicKey...),
		AttestationType: c.AttestationType,
		Transports:      transports,
		AAGUID:          append([]byte(nil), c.Authenticator.AAGUID...),
		BackupEligible:  c.Flags.BackupEligible,
		BackupState:     c.Flags.BackupState,
	}
}

// NewWebAuthn builds a *webauthn.WebAuthn for the given rp-id/origins/display
// name. An empty origins list defaults to https://<rpID>, per the management
// login design's origin check.
func NewWebAuthn(rpID, displayName string, origins []string) (*webauthn.WebAuthn, error) {
	rpID = strings.TrimSpace(rpID)
	if rpID == "" {
		return nil, ErrPasskeysDisabled
	}
	resolvedOrigins := origins
	if len(resolvedOrigins) == 0 {
		resolvedOrigins = []string{"https://" + rpID}
	}
	if displayName == "" {
		displayName = rpID
	}
	return webauthn.New(&webauthn.Config{
		RPID:          rpID,
		RPDisplayName: displayName,
		RPOrigins:     resolvedOrigins,
	})
}

// BeginAddPasskey returns WebAuthn creation options and session data for
// registering a new passkey: resident key required, user verification
// required, with the account's existing passkeys excluded so a device
// cannot register the same passkey twice.
func BeginAddPasskey(w *webauthn.WebAuthn, user User) (*protocol.CredentialCreation, *webauthn.SessionData, error) {
	exclude := make([]protocol.CredentialDescriptor, 0, len(user.Credentials))
	for _, cred := range user.Credentials {
		exclude = append(exclude, protocol.CredentialDescriptor{
			Type:         protocol.PublicKeyCredentialType,
			CredentialID: cred.ID,
		})
	}
	requireResidentKey := true
	return w.BeginRegistration(user,
		webauthn.WithAuthenticatorSelection(protocol.AuthenticatorSelection{
			ResidentKey:        protocol.ResidentKeyRequirementRequired,
			RequireResidentKey: &requireResidentKey,
			UserVerification:   protocol.VerificationRequired,
		}),
		webauthn.WithExclusions(exclude),
	)
}

// FinishAddPasskey validates a registration response against session and
// returns the resulting credential.
func FinishAddPasskey(w *webauthn.WebAuthn, user User, session webauthn.SessionData, credentialJSON []byte) (*webauthn.Credential, error) {
	parsed, err := protocol.ParseCredentialCreationResponseBytes(credentialJSON)
	if err != nil {
		return nil, err
	}
	return w.CreateCredential(user, session, parsed)
}

// BeginPasskeyLogin returns assertion options for a discoverable (usernameless)
// passkey login: no allowCredentials, so the authenticator itself picks the
// credential to present, with user verification required.
func BeginPasskeyLogin(w *webauthn.WebAuthn) (*protocol.CredentialAssertion, *webauthn.SessionData, error) {
	return w.BeginDiscoverableLogin(webauthn.WithUserVerification(protocol.VerificationRequired))
}

// FinishPasskeyLogin validates a discoverable login response, resolving the
// signing user through lookup.
func FinishPasskeyLogin(w *webauthn.WebAuthn, lookup webauthn.DiscoverableUserHandler, session webauthn.SessionData, credentialJSON []byte) (webauthn.User, *webauthn.Credential, error) {
	parsed, err := protocol.ParseCredentialRequestResponseBytes(credentialJSON)
	if err != nil {
		return nil, nil, err
	}
	return w.ValidatePasskeyLogin(lookup, session, parsed)
}

// Ceremony is a pending WebAuthn registration or login challenge.
type Ceremony struct {
	Session   webauthn.SessionData
	ExpiresAt time.Time
}

// MaxPendingCeremonies caps the number of outstanding ceremonies: once
// reached, Begin evicts the oldest pending ceremony to make room rather than
// growing further or rejecting the new one.
const MaxPendingCeremonies = 256

// ceremonyPurgeMinInterval bounds how often Begin scans for expired entries
// when the cache is not already at capacity, so a steady trickle of begins
// does not pay an O(n) purge scan on every single call.
const ceremonyPurgeMinInterval = 10 * time.Second

// CeremonyCache holds single-use, TTL-bound WebAuthn ceremonies in memory
// only, as the management login design requires: no persistence, so a
// restart mid-ceremony just means pressing the button again. Login and
// registration ceremonies are expected to live in separate CeremonyCache
// instances (see Handler), so a burst of one kind can never evict or starve
// the other.
type CeremonyCache struct {
	clock Clock

	mu        sync.Mutex
	items     map[string]Ceremony
	order     []string // insertion order, oldest first; may contain stale ids
	lastPurge time.Time
}

// NewCeremonyCache creates an empty cache driven by clock. A nil clock uses
// SystemClock.
func NewCeremonyCache(clock Clock) *CeremonyCache {
	if clock == nil {
		clock = SystemClock{}
	}
	return &CeremonyCache{clock: clock, items: make(map[string]Ceremony)}
}

// Begin stores session under a fresh random ceremony id and returns the id.
// Once MaxPendingCeremonies are outstanding (even after an expiry purge),
// the oldest pending ceremony is evicted to make room: an attacker spamming
// begin cannot grow the cache without bound, but also cannot use it to deny
// service by exhausting it, since the real cost of eviction falls on the
// least-recently-started (most likely already-abandoned) ceremony.
func (c *CeremonyCache) Begin(session webauthn.SessionData) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.clock.Now()
	if len(c.items) >= MaxPendingCeremonies || now.Sub(c.lastPurge) >= ceremonyPurgeMinInterval {
		c.purgeExpiredLocked(now)
		c.lastPurge = now
	}
	if len(c.items) >= MaxPendingCeremonies {
		c.evictOldestLocked()
	}

	id, err := randomCeremonyID()
	if err != nil {
		return "", err
	}
	c.items[id] = Ceremony{Session: session, ExpiresAt: now.Add(CeremonyTTL)}
	c.order = append(c.order, id)
	return id, nil
}

// Take removes and returns the ceremony for id. The ceremony is always
// single-use: a lookup consumes it whether or not the caller goes on to
// finish it successfully. The second return value is false when id is
// unknown or its TTL has expired.
func (c *CeremonyCache) Take(id string) (webauthn.SessionData, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ceremony, ok := c.items[id]
	delete(c.items, id)
	if !ok {
		return webauthn.SessionData{}, false
	}
	if !c.clock.Now().Before(ceremony.ExpiresAt) {
		return webauthn.SessionData{}, false
	}
	return ceremony.Session, true
}

func (c *CeremonyCache) purgeExpiredLocked(now time.Time) {
	for id, ceremony := range c.items {
		if !now.Before(ceremony.ExpiresAt) {
			delete(c.items, id)
		}
	}
	c.trimStaleOrderPrefixLocked()
}

// evictOldestLocked removes the single oldest still-pending ceremony,
// skipping over any ids at the front of order that were already removed by
// Take or a purge.
func (c *CeremonyCache) evictOldestLocked() {
	for len(c.order) > 0 {
		oldest := c.order[0]
		c.order = c.order[1:]
		if _, ok := c.items[oldest]; ok {
			delete(c.items, oldest)
			return
		}
	}
}

// trimStaleOrderPrefixLocked drops the leading run of order entries that no
// longer exist in items, bounding order's growth without needing to scan or
// rewrite the whole slice on every Take.
func (c *CeremonyCache) trimStaleOrderPrefixLocked() {
	i := 0
	for i < len(c.order) {
		if _, ok := c.items[c.order[i]]; ok {
			break
		}
		i++
	}
	if i > 0 {
		c.order = c.order[i:]
	}
}

func randomCeremonyID() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("mgmtauth: generate ceremony id: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
