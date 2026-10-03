package slackbridge

// maintain is the bridge's periodic housekeeping: it refreshes link
// liveness from the bus and drops the links of absent sessions.
func (b *Bridge) maintain() {
	b.refreshLinks()
}

// flush writes the state file now.
func (st *state) flush() error {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.saveLocked()
}
