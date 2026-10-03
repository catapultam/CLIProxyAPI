package management

// Minimal software WebAuthn authenticator for exercising the passkey
// account/session routes end to end over HTTP, without a real device. This
// duplicates the equivalent helper in internal/mgmtauth's tests: the two
// packages cannot share unexported test helpers across a package boundary,
// and this is test-only code.

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

	"github.com/fxamacker/cbor/v2"
)

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

func (a *softAuthenticator) authenticatorData(t *testing.T, rpID string, attested, backupEligible, backupState bool) []byte {
	t.Helper()
	var buf bytes.Buffer
	rpHash := sha256.Sum256([]byte(rpID))
	buf.Write(rpHash[:])

	var flags byte = 0x01 | 0x04 // UP, UV
	if backupEligible {
		flags |= 0x08
	}
	if backupState {
		flags |= 0x10
	}
	if attested {
		flags |= 0x40
	}
	buf.WriteByte(flags)
	buf.Write(make([]byte, 4)) // counter = 0

	if attested {
		buf.Write(make([]byte, 16)) // zero AAGUID
		credIDLen := make([]byte, 2)
		binary.BigEndian.PutUint16(credIDLen, uint16(len(a.credentialID)))
		buf.Write(credIDLen)
		buf.Write(a.credentialID)
		buf.Write(a.cosePublicKey(t))
	}
	return buf.Bytes()
}

func webauthnClientDataJSON(t *testing.T, ceremonyType, challenge, origin string) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]string{"type": ceremonyType, "challenge": challenge, "origin": origin})
	if err != nil {
		t.Fatalf("marshal clientDataJSON: %v", err)
	}
	return raw
}

func webauthnB64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func (a *softAuthenticator) registrationResponseJSON(t *testing.T, rpID, origin string, challenge []byte) []byte {
	t.Helper()
	authData := a.authenticatorData(t, rpID, true, true, true)
	attestationObject, err := cbor.Marshal(map[string]interface{}{
		"fmt":      "none",
		"attStmt":  map[string]interface{}{},
		"authData": authData,
	})
	if err != nil {
		t.Fatalf("marshal attestationObject: %v", err)
	}
	cdj := webauthnClientDataJSON(t, "webauthn.create", webauthnB64(challenge), origin)
	payload := map[string]interface{}{
		"id":    webauthnB64(a.credentialID),
		"rawId": webauthnB64(a.credentialID),
		"type":  "public-key",
		"response": map[string]interface{}{
			"attestationObject": webauthnB64(attestationObject),
			"clientDataJSON":    webauthnB64(cdj),
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal registration response: %v", err)
	}
	return raw
}

func (a *softAuthenticator) assertionResponseJSON(t *testing.T, rpID, origin string, challenge, userHandle []byte) []byte {
	t.Helper()
	authData := a.authenticatorData(t, rpID, false, true, true)
	cdj := webauthnClientDataJSON(t, "webauthn.get", webauthnB64(challenge), origin)
	cdjHash := sha256.Sum256(cdj)
	signed := append(append([]byte(nil), authData...), cdjHash[:]...)
	digest := sha256.Sum256(signed)
	sig, err := ecdsa.SignASN1(rand.Reader, a.key, digest[:])
	if err != nil {
		t.Fatalf("sign assertion: %v", err)
	}
	payload := map[string]interface{}{
		"id":    webauthnB64(a.credentialID),
		"rawId": webauthnB64(a.credentialID),
		"type":  "public-key",
		"response": map[string]interface{}{
			"authenticatorData": webauthnB64(authData),
			"clientDataJSON":    webauthnB64(cdj),
			"signature":         webauthnB64(sig),
			"userHandle":        webauthnB64(userHandle),
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal assertion response: %v", err)
	}
	return raw
}
