// Package mgmtauth implements the building blocks for the management panel
// login feature: password hashing, stateless session tokens, a WebAuthn
// (passkey) wrapper with an in-memory ceremony cache, and login throttling.
// See docs/superpowers/specs/2026-10-02-management-login-design.md for the
// backend design this package implements.
package mgmtauth

import "time"

// Clock abstracts wall-clock time so token expiry, sliding refresh and login
// throttling can be tested deterministically without time.Sleep.
type Clock interface {
	Now() time.Time
}

// SystemClock is the production Clock backed by time.Now.
type SystemClock struct{}

// Now returns the current wall-clock time.
func (SystemClock) Now() time.Time { return time.Now() }
