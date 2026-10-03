package mgmtauth

import "time"

// mockClock is a controllable Clock for deterministic tests. It never calls
// time.Sleep; callers advance it explicitly.
type mockClock struct {
	now time.Time
}

func newMockClock(now time.Time) *mockClock {
	return &mockClock{now: now}
}

func (c *mockClock) Now() time.Time { return c.now }

func (c *mockClock) Advance(d time.Duration) { c.now = c.now.Add(d) }
