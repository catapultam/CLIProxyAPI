package auth

import "testing"

// TestCodexPlanSizeMapsKnownTiersCaseInsensitively covers the owner-provided
// Codex scale (see codexPlanSizes): plus and team are seat-equivalent (1),
// prolite is the x5 tier, and plan types outside the table (pro, free, a
// missing plan_type) report unknown rather than guessing a size.
func TestCodexPlanSizeMapsKnownTiersCaseInsensitively(t *testing.T) {
	for _, tc := range []struct {
		name     string
		planType string
		want     float64
		wantOK   bool
	}{
		{"plus lowercase", "plus", 1, true},
		{"PLUS uppercase", "PLUS", 1, true},
		{"team", "team", 1, true},
		{"Team mixed case", "Team", 1, true},
		{"prolite", "prolite", 5, true},
		{"ProLite mixed case", "ProLite", 5, true},
		{"pro is not prolite", "pro", 0, false},
		{"free", "free", 0, false},
		{"enterprise", "enterprise", 0, false},
		{"missing", "", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			auth := &Auth{Provider: "codex", Attributes: map[string]string{}}
			if tc.planType != "" {
				auth.Attributes["plan_type"] = tc.planType
			}
			got, ok := codexPlanSize(auth)
			if ok != tc.wantOK || (ok && got != tc.want) {
				t.Fatalf("codexPlanSize(plan_type=%q) = %v, %v; want %v, %v", tc.planType, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// TestCodexPlanSizeFallsBackToMetadata covers a credential whose plan_type
// only lives in Metadata (no routing attribute yet), the same fallback shape
// authSize itself uses for "size".
func TestCodexPlanSizeFallsBackToMetadata(t *testing.T) {
	auth := &Auth{Provider: "codex", Metadata: map[string]any{"plan_type": "prolite"}}
	got, ok := codexPlanSize(auth)
	if !ok || got != 5 {
		t.Fatalf("codexPlanSize() = %v, %v; want 5, true", got, ok)
	}
}

// claudeProfileFixture builds a minimal profile JSON body with only the
// fields parseClaudeProfileSize reads.
func claudeProfileFixture(hasClaudePro bool, orgType, rateLimitTier string) string {
	return `{"account":{"has_claude_max":false,"has_claude_pro":` + boolJSON(hasClaudePro) + `},` +
		`"organization":{"organization_type":"` + orgType + `","rate_limit_tier":"` + rateLimitTier + `","subscription_status":"active"}}`
}

func boolJSON(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// TestParseClaudeProfileSizeMapsTiers covers every mapping rule from the
// Claude plan-size spec: a rate_limit_tier naming the 20x or 5x multiplier
// wins outright; a Pro or Team organization is size 1, by either
// organization_type or account.has_claude_pro; anything else is unknown.
func TestParseClaudeProfileSizeMapsTiers(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want float64
		ok   bool
	}{
		{"max_20x tier", claudeProfileFixture(true, "claude_max", "default_claude_max_20x"), 20, true},
		{"max_5x tier", claudeProfileFixture(true, "claude_max", "default_claude_max_5x"), 5, true},
		{"pro via organization_type", claudeProfileFixture(false, "claude_pro", "default"), 1, true},
		{"team via organization_type", claudeProfileFixture(false, "claude_team", "default"), 1, true},
		{"pro via has_claude_pro", claudeProfileFixture(true, "claude_free", "default"), 1, true},
		{"free, no pro flag", claudeProfileFixture(false, "claude_free", "default"), 0, false},
		{"enterprise", claudeProfileFixture(false, "claude_enterprise", "default"), 0, false},
		{"unrecognized tier string falls back to org fields", claudeProfileFixture(false, "claude_free", "some_future_tier"), 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok, errParse := parseClaudeProfileSize([]byte(tc.body))
			if errParse != nil {
				t.Fatalf("parseClaudeProfileSize: %v", errParse)
			}
			if ok != tc.ok || (ok && got != tc.want) {
				t.Fatalf("parseClaudeProfileSize(%s) = %v, %v; want %v, %v", tc.body, got, ok, tc.want, tc.ok)
			}
		})
	}
}

// TestParseClaudeProfileSizeMalformedJSON reports an error, not a silent
// "unknown", so a caller backs off and retries instead of latching in a
// false "no plan" result.
func TestParseClaudeProfileSizeMalformedJSON(t *testing.T) {
	if _, _, err := parseClaudeProfileSize([]byte("{not json")); err == nil {
		t.Fatal("malformed body did not report an error")
	}
}

// TestNextResetPlanSizeStorePrune covers Fix 5: pruning drops entries for
// auth IDs outside keep, in memory, and leaves tracked IDs untouched.
func TestNextResetPlanSizeStorePrune(t *testing.T) {
	s := newNextResetPlanSizeStore()
	s.set("kept", nextResetPlanSizeEntry{Size: 1, Known: true})
	s.set("gone", nextResetPlanSizeEntry{Size: 5, Known: true})
	nextResetStateDirty.Store(false)

	s.prune(map[string]bool{"kept": true})

	if _, ok := s.get("gone"); ok {
		t.Fatal("prune left an untracked auth ID's plan size in memory")
	}
	if entry, ok := s.get("kept"); !ok || !entry.Known || entry.Size != 1 {
		t.Fatalf("prune dropped a tracked auth ID's plan size: %+v %v", entry, ok)
	}
	if !nextResetStateDirty.Load() {
		t.Fatal("prune did not mark state dirty so the pruned store persists")
	}
}

// TestNextResetPlanSizeStorePruneNoopWhenNothingStale covers that pruning an
// already-clean store leaves nextResetStateDirty untouched.
func TestNextResetPlanSizeStorePruneNoopWhenNothingStale(t *testing.T) {
	s := newNextResetPlanSizeStore()
	s.set("kept", nextResetPlanSizeEntry{Size: 1, Known: true})
	nextResetStateDirty.Store(false)

	s.prune(map[string]bool{"kept": true})

	if nextResetStateDirty.Load() {
		t.Fatal("prune marked state dirty despite dropping nothing")
	}
}

// TestTrackedAuthIDSet covers the manager-side half of Fix 5: the set
// reflects exactly the manager's currently registered auth IDs.
func TestTrackedAuthIDSet(t *testing.T) {
	m := newQuotaSummaryTestManager(&Auth{ID: "a", Provider: "claude"}, &Auth{ID: "b", Provider: "codex"})
	ids := trackedAuthIDSet(m)
	if len(ids) != 2 || !ids["a"] || !ids["b"] {
		t.Fatalf("trackedAuthIDSet = %v, want {a, b}", ids)
	}
}
