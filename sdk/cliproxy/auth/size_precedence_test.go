package auth

import "testing"

// withPlanSizeStore isolates nextResetPlanSizes (the package-level Claude
// plan-size store; see size_plan.go) for one test: it swaps in a fresh store
// and restores whatever was there before on cleanup, so a test that derives
// a Claude size for a reused auth ID (next-reset tests reuse "a", "b", "c"
// freely) cannot leak into, or be polluted by, any other test in this
// package.
func withPlanSizeStore(t *testing.T) {
	t.Helper()
	prev := nextResetPlanSizes
	nextResetPlanSizes = newNextResetPlanSizeStore()
	t.Cleanup(func() { nextResetPlanSizes = prev })
}

// TestAuthSizeCodexPlanOverriddenByHandSetSize covers the top of the
// precedence: a hand-set attribute size wins over the credential's plan
// even when they disagree.
func TestAuthSizeCodexPlanOverriddenByHandSetSize(t *testing.T) {
	auth := &Auth{Provider: "codex", Attributes: map[string]string{AttributeSize: "2", "plan_type": "prolite"}}
	got, ok, source := authSizeWithSource(auth)
	if !ok || got != 2 || source != "hand" {
		t.Fatalf("authSizeWithSource() = %v, %v, %q; want 2, true, hand", got, ok, source)
	}
}

// TestAuthSizeCodexPlanUsedWhenNoHandSetSize covers the new third step: no
// attribute, no metadata size, so the Codex plan map decides.
func TestAuthSizeCodexPlanUsedWhenNoHandSetSize(t *testing.T) {
	auth := &Auth{Provider: "codex", Attributes: map[string]string{"plan_type": "team"}}
	got, ok, source := authSizeWithSource(auth)
	if !ok || got != 1 || source != "plan" {
		t.Fatalf("authSizeWithSource() = %v, %v, %q; want 1, true, plan", got, ok, source)
	}
}

// TestAuthSizeClaudePlanUnknownWithoutDerivedEntry covers a Claude
// credential the poller has not derived a plan size for yet: unknown, not a
// false size=0.
func TestAuthSizeClaudePlanUnknownWithoutDerivedEntry(t *testing.T) {
	withPlanSizeStore(t)
	auth := &Auth{ID: "no-plan-entry", Provider: "claude"}
	if size, ok, source := authSizeWithSource(auth); ok || source != "" || size != 0 {
		t.Fatalf("authSizeWithSource() = %v, %v, %q; want 0, false, \"\"", size, ok, source)
	}
}

// TestAuthSizePrecedenceAttributeMetadataPlan exercises all three steps in
// order on one credential: the attribute wins while present, removing it
// falls back to metadata, and removing that falls back to the plan-derived
// store.
func TestAuthSizePrecedenceAttributeMetadataPlan(t *testing.T) {
	withPlanSizeStore(t)
	auth := &Auth{
		ID:         "precedence-claude",
		Provider:   "claude",
		Attributes: map[string]string{AttributeSize: "2"},
		Metadata:   map[string]any{AttributeSize: float64(7)},
	}
	nextResetPlanSizes.set(auth.ID, nextResetPlanSizeEntry{Size: 20, Known: true})

	if got, ok, source := authSizeWithSource(auth); !ok || got != 2 || source != "hand" {
		t.Fatalf("attribute step = %v, %v, %q; want 2, true, hand", got, ok, source)
	}

	delete(auth.Attributes, AttributeSize)
	if got, ok, source := authSizeWithSource(auth); !ok || got != 7 || source != "hand" {
		t.Fatalf("metadata step = %v, %v, %q; want 7, true, hand", got, ok, source)
	}

	delete(auth.Metadata, AttributeSize)
	if got, ok, source := authSizeWithSource(auth); !ok || got != 20 || source != "plan" {
		t.Fatalf("plan step = %v, %v, %q; want 20, true, plan", got, ok, source)
	}
}

// TestAuthSizeInvalidAttributeDoesNotFallThroughToPlan covers the existing
// all-or-nothing rule at the attribute step (TestAuthSizeAttributeWinsOverMetadata
// covers it against metadata): an invalid attribute reports unknown even
// when a plan-derived size is available, rather than silently preferring
// the plan.
func TestAuthSizeInvalidAttributeDoesNotFallThroughToPlan(t *testing.T) {
	auth := &Auth{
		Provider:   "codex",
		Attributes: map[string]string{AttributeSize: "not-a-number", "plan_type": "team"},
	}
	if _, ok, source := authSizeWithSource(auth); ok || source != "" {
		t.Fatalf("invalid attribute fell through to the plan: ok=%v source=%q", ok, source)
	}
}
