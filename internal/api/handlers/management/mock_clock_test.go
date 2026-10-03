package management

import "time"

// mockClock is a controllable mgmtauth.Clock for deterministic tests
// (login throttling, session expiry/refresh) without time.Sleep.
type mockClock struct {
	now time.Time
}

func newMockClock(now time.Time) *mockClock {
	return &mockClock{now: now}
}

func (c *mockClock) Now() time.Time { return c.now }

func (c *mockClock) Advance(d time.Duration) { c.now = c.now.Add(d) }
