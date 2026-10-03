package mgmtauth

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/go-webauthn/webauthn/webauthn"
)

// softAuthenticator is a minimal software WebAuthn authenticator used to
// exercise the mgmtauth passkey wrapper end to end without a real device.
// It speaks ES256 (COSE algorithm -7) over the P-256 curve, which is what
// every mainstream platform authenticator and browser also default to.
type softAuthenticator struct {
	key          *ecdsa.PrivateKey
	credentialID []byte
}

func newSoftAuthenticator(t *testing.T, credentialID []byte) *softAuthenticator {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate ES256 key: %v", err)
	}
	return &softAuthenticator{key: key, credentialID: credentialID}
}

// coseKey is the COSE_Key encoding of an EC2/ES256 public key, with CBOR
// labels matching protocol/webauthncose.EC2PublicKeyData exactly.
type coseKey struct {
	KeyType   int64  `cbor:"1,keyasint"`
	Algorithm int64  `cbor:"3,keyasint"`
	Curve     int64  `cbor:"-1,keyasint"`
	XCoord    []byte `cbor:"-2,keyasint"`
	YCoord    []byte `cbor:"-3,keyasint"`
}

func (a *softAuthenticator) cosePublicKey(t *testing.T) []byte {
	t.Helper()
	x := a.key.PublicKey.X.FillBytes(make([]byte, 32))
	y := a.key.PublicKey.Y.FillBytes(make([]byte, 32))
	raw, err := cbor.Marshal(coseKey{KeyType: 2, Algorithm: -7, Curve: 1, XCoord: x, YCoord: y})
	if err != nil {
		t.Fatalf("marshal COSE key: %v", err)
	}
	return raw
}

// authenticatorData builds the raw (non-CBOR) authenticator data structure:
// rpIdHash(32) || flags(1) || counter(4) [|| attestedCredentialData].
// attested is non-nil only for the registration ceremony.
func (a *softAuthenticator) authenticatorData(t *testing.T, rpID string, attested bool, backupEligible, backupState bool) []byte {
	t.Helper()
	var buf bytes.Buffer
	rpHash := sha256.Sum256([]byte(rpID))
	buf.Write(rpHash[:])

	var flags byte = 0x01 // UP: user present
	flags |= 0x04         // UV: user verified
	if backupEligible {
		flags |= 0x08 // BE
	}
	if backupState {
		flags |= 0x10 // BS
	}
	if attested {
		flags |= 0x40 // AT: attested credential data included
	}
	buf.WriteByte(flags)

	counter := make([]byte, 4)
	binary.BigEndian.PutUint32(counter, 0)
	buf.Write(counter)

	if attested {
		buf.Write(make([]byte, 16)) // AAGUID: zero, "no particular authenticator model"
		credIDLen := make([]byte, 2)
		binary.BigEndian.PutUint16(credIDLen, uint16(len(a.credentialID)))
		buf.Write(credIDLen)
		buf.Write(a.credentialID)
		buf.Write(a.cosePublicKey(t))
	}

	return buf.Bytes()
}

func clientDataJSON(t *testing.T, ceremonyType, challenge, origin string) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]string{
		"type":      ceremonyType,
		"challenge": challenge,
		"origin":    origin,
	})
	if err != nil {
		t.Fatalf("marshal clientDataJSON: %v", err)
	}
	return raw
}

func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// registrationResponseJSON builds the raw bytes a browser's
// credential.toJSON() would produce for navigator.credentials.create().
func (a *softAuthenticator) registrationResponseJSON(t *testing.T, rpID, origin string, challenge []byte, backupEligible, backupState bool) []byte {
	t.Helper()
	authData := a.authenticatorData(t, rpID, true, backupEligible, backupState)
	attestationObject, err := cbor.Marshal(map[string]interface{}{
		"fmt":      "none",
		"attStmt":  map[string]interface{}{},
		"authData": authData,
	})
	if err != nil {
		t.Fatalf("marshal attestationObject: %v", err)
	}
	cdj := clientDataJSON(t, "webauthn.create", b64url(challenge), origin)

	payload := map[string]interface{}{
		"id":    b64url(a.credentialID),
		"rawId": b64url(a.credentialID),
		"type":  "public-key",
		"response": map[string]interface{}{
			"attestationObject": b64url(attestationObject),
			"clientDataJSON":    b64url(cdj),
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal registration response: %v", err)
	}
	return raw
}

// assertionResponseJSON builds the raw bytes for navigator.credentials.get().
func (a *softAuthenticator) assertionResponseJSON(t *testing.T, rpID, origin string, challenge, userHandle []byte, backupEligible, backupState bool) []byte {
	t.Helper()
	authData := a.authenticatorData(t, rpID, false, backupEligible, backupState)
	cdj := clientDataJSON(t, "webauthn.get", b64url(challenge), origin)
	cdjHash := sha256.Sum256(cdj)

	signed := append(append([]byte(nil), authData...), cdjHash[:]...)
	sig, err := ecdsa.SignASN1(rand.Reader, a.key, mustHash(signed))
	if err != nil {
		t.Fatalf("sign assertion: %v", err)
	}

	payload := map[string]interface{}{
		"id":    b64url(a.credentialID),
		"rawId": b64url(a.credentialID),
		"type":  "public-key",
		"response": map[string]interface{}{
			"authenticatorData": b64url(authData),
			"clientDataJSON":    b64url(cdj),
			"signature":         b64url(sig),
			"userHandle":        b64url(userHandle),
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal assertion response: %v", err)
	}
	return raw
}

func mustHash(data []byte) []byte {
	sum := sha256.Sum256(data)
	return sum[:]
}

const testRPID = "mgmt.example.com"
const testOrigin = "https://mgmt.example.com"

func newTestWebAuthn(t *testing.T, origins []string) *webauthn.WebAuthn {
	t.Helper()
	w, err := NewWebAuthn(testRPID, "CLIProxyAPI", origins)
	if err != nil {
		t.Fatalf("NewWebAuthn: %v", err)
	}
	return w
}

// TestPasskeyRegistrationAndLoginEndToEnd drives a full add-passkey then
// discoverable-login ceremony using a software ES256 authenticator, matching
// what a real platform authenticator and browser would send.
func TestPasskeyRegistrationAndLoginEndToEnd(t *testing.T) {
	w := newTestWebAuthn(t, nil)
	handle := []byte("user-handle-0123456789abcdef012")
	user := User{Handle: handle, Username: "admin"}

	creation, session, err := BeginAddPasskey(w, user)
	if err != nil {
		t.Fatalf("BeginAddPasskey: %v", err)
	}

	auth := newSoftAuthenticator(t, []byte("credential-id-1"))
	regJSON := auth.registrationResponseJSON(t, testRPID, testOrigin, creation.Response.Challenge, true, true)

	cred, err := FinishAddPasskey(w, user, *session, regJSON)
	if err != nil {
		t.Fatalf("FinishAddPasskey: %v", err)
	}
	if !bytes.Equal(cred.ID, auth.credentialID) {
		t.Fatalf("credential ID = %x, want %x", cred.ID, auth.credentialID)
	}

	stored := FromWebAuthnCredential(cred)
	user.Credentials = append(user.Credentials, stored)

	// Discoverable login.
	assertion, loginSession, err := BeginPasskeyLogin(w)
	if err != nil {
		t.Fatalf("BeginPasskeyLogin: %v", err)
	}

	lookup := func(rawID, userHandle []byte) (webauthn.User, error) {
		if !bytes.Equal(userHandle, user.WebAuthnID()) {
			t.Fatalf("lookup called with unexpected user handle %x", userHandle)
		}
		return user, nil
	}

	loginJSON := auth.assertionResponseJSON(t, testRPID, testOrigin, assertion.Response.Challenge, handle, true, true)
	gotUser, gotCred, err := FinishPasskeyLogin(w, lookup, *loginSession, loginJSON)
	if err != nil {
		t.Fatalf("FinishPasskeyLogin: %v", err)
	}
	if gotUser.WebAuthnName() != "admin" {
		t.Fatalf("resolved user = %q, want admin", gotUser.WebAuthnName())
	}
	if !bytes.Equal(gotCred.ID, auth.credentialID) {
		t.Fatalf("logged-in credential ID = %x, want %x", gotCred.ID, auth.credentialID)
	}
}

// TestPasskeyLoginRejectsUnknownOrigin confirms that an origin absent from
// passkey-origins is rejected, per the management login design's origin
// check.
func TestPasskeyLoginRejectsUnknownOrigin(t *testing.T) {
	allowedOrigin := "https://allowed.example.com"
	w := newTestWebAuthn(t, []string{allowedOrigin})
	handle := []byte("user-handle-0123456789abcdef012")
	user := User{Handle: handle, Username: "admin"}

	creation, session, err := BeginAddPasskey(w, user)
	if err != nil {
		t.Fatalf("BeginAddPasskey: %v", err)
	}
	auth := newSoftAuthenticator(t, []byte("credential-id-2"))
	// Register from the allowed origin so the credential itself is valid...
	regJSON := auth.registrationResponseJSON(t, testRPID, allowedOrigin, creation.Response.Challenge, true, true)
	cred, err := FinishAddPasskey(w, user, *session, regJSON)
	if err != nil {
		t.Fatalf("FinishAddPasskey: %v", err)
	}
	user.Credentials = append(user.Credentials, FromWebAuthnCredential(cred))

	assertion, loginSession, err := BeginPasskeyLogin(w)
	if err != nil {
		t.Fatalf("BeginPasskeyLogin: %v", err)
	}
	lookup := func(rawID, userHandle []byte) (webauthn.User, error) { return user, nil }

	// ...but attempt the login from an origin that is not in passkey-origins.
	disallowedOrigin := "https://not-allowed.example.com"
	loginJSON := auth.assertionResponseJSON(t, testRPID, disallowedOrigin, assertion.Response.Challenge, handle, true, true)
	if _, _, err := FinishPasskeyLogin(w, lookup, *loginSession, loginJSON); err == nil {
		t.Fatal("expected FinishPasskeyLogin to reject an origin outside passkey-origins")
	}
}

// TestCeremonyCacheSingleUse verifies a ceremony id can only be consumed
// once, as the management login design requires.
func TestCeremonyCacheSingleUse(t *testing.T) {
	clock := newMockClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	cache := NewCeremonyCache(clock)

	id, err := cache.Begin(webauthn.SessionData{Challenge: "abc"})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	session, ok := cache.Take(id)
	if !ok || session.Challenge != "abc" {
		t.Fatalf("first Take() = (%v, %v), want ({Challenge: abc}, true)", session, ok)
	}

	if _, ok := cache.Take(id); ok {
		t.Fatal("expected second Take() of the same ceremony id to fail")
	}
}

// TestCeremonyCacheExpires verifies a ceremony older than CeremonyTTL cannot
// be consumed, using the injected clock rather than time.Sleep.
func TestCeremonyCacheExpires(t *testing.T) {
	clock := newMockClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	cache := NewCeremonyCache(clock)

	id, err := cache.Begin(webauthn.SessionData{Challenge: "abc"})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	clock.Advance(CeremonyTTL + time.Second)
	if _, ok := cache.Take(id); ok {
		t.Fatal("expected an expired ceremony to be rejected")
	}
}

func TestCeremonyCacheUnknownID(t *testing.T) {
	cache := NewCeremonyCache(nil)
	if _, ok := cache.Take("does-not-exist"); ok {
		t.Fatal("expected an unknown ceremony id to be rejected")
	}
}

// TestCeremonyCacheCapsPendingCeremonies verifies Begin refuses to grow the
// cache past MaxPendingCeremonies, protecting against an unbounded-memory
// DoS from a client that only ever calls begin.
func TestCeremonyCacheCapsPendingCeremonies(t *testing.T) {
	clock := newMockClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	cache := NewCeremonyCache(clock)

	for i := 0; i < MaxPendingCeremonies; i++ {
		if _, err := cache.Begin(webauthn.SessionData{Challenge: "abc"}); err != nil {
			t.Fatalf("Begin() #%d: %v", i, err)
		}
	}

	if _, err := cache.Begin(webauthn.SessionData{Challenge: "overflow"}); err != ErrTooManyCeremonies {
		t.Fatalf("Begin() at capacity: err = %v, want ErrTooManyCeremonies", err)
	}

	// Expiring everything and advancing past the cache's internal purge
	// interval frees room again; a purge can happen even without reaching
	// the interval once the cache is at capacity.
	clock.Advance(CeremonyTTL + time.Second)
	if _, err := cache.Begin(webauthn.SessionData{Challenge: "after-purge"}); err != nil {
		t.Fatalf("Begin() after expiry: %v", err)
	}
}
