package auth

import (
	"context"
	"math"
	"testing"
	"time"
)

// withCodexPlan sets a's plan_type routing attribute (see codexPlanSize) and
// returns a, for building plan-derived-size ranking test fixtures. Unlike
// sized (selector_next_reset_test.go), this leaves AttributeSize untouched,
// so authSize must fall all the way through to planDerivedSize for these
// accounts to get any size bias at all.
func withCodexPlan(a *Auth, planType string) *Auth {
	if a.Attributes == nil {
		a.Attributes = map[string]string{}
	}
	a.Attributes["plan_type"] = planType
	return a
}

// TestNextResetCodexTeamPlanSizeGets24hShiftRelativeToProlite is the
// ranking end-to-end proof that a plan-derived size feeds the same bias a
// hand-set size does (see TestNextResetSmallerAccountWithResetUpTo24hLaterBeatsLarger
// for the hand-set equivalent). prolite (size 5, the x5 tier) is the
// largest known size among these two tracked Codex accounts, so it never
// shifts. team (size 1) is smaller and would lose on real reset time alone
// (30h vs prolite's 20h), but the flat 24h shift puts its effective reset
// at 6h, ahead of prolite's real 20h. Neither account carries a hand-set
// size attribute: both derive their size purely from plan_type.
func TestNextResetCodexTeamPlanSizeGets24hShiftRelativeToProlite(t *testing.T) {
	prolite := withCodexPlan(codexAuth("prolite-acct", 50, 20*time.Hour), "prolite")
	team := withCodexPlan(codexAuth("team-acct", 50, 30*time.Hour), "team")
	if got := nrPick(t, nrSelectorTrackingAll(t, prolite, team), "gpt-5.5", prolite, team); got.ID != "team-acct" {
		t.Fatalf("got %s, want team-acct (plan-derived size shifts it within 24h of prolite)", got.ID)
	}
}

// TestPooledUsageForProviderWeightsByCodexPlanDerivedSizeWhenAllKnown is the
// plan-derived-size equivalent of TestPooledUsageForProviderWeightsBySizeWhenAllKnown:
// pooled usage is weighted by size even when neither account has a hand-set
// size attribute, because authSize falls through to the Codex plan map.
func TestPooledUsageForProviderWeightsByCodexPlanDerivedSizeWhenAllKnown(t *testing.T) {
	withNextReset(t)
	m := NewManager(nil, nil, nil)
	small := withCodexPlan(codexAuth("plan-sz-small", 80, 48*time.Hour), "team") // size 1
	big := withCodexPlan(codexAuth("plan-sz-big", 10, 24*time.Hour), "prolite")  // size 5
	for _, a := range []*Auth{small, big} {
		if _, errRegister := m.Register(context.Background(), a); errRegister != nil {
			t.Fatal(errRegister)
		}
	}
	// Plain mean would be 45%; size-weighted mean is (1*80 + 5*10)/6.
	want := (1*80.0 + 5*10.0) / 6
	got := m.PooledUsageForProvider("codex", nrNow)
	if got.Accounts != 2 || got.SevenDay == nil || math.Abs(got.SevenDay.UsedPercentage-want) > 1e-6 {
		t.Fatalf("weighted pool = %+v %+v, want %v", got, got.SevenDay, want)
	}
}
