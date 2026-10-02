package auth

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

func TestPooledAccumulatorMeansAndEarliestReset(t *testing.T) {
	var a pooledAccumulator
	a.add(nextResetWindow{Known: true, UsedPct: 20, ResetsAt: nrNow.Add(48 * time.Hour)}, false, nrNow)
	a.add(nextResetWindow{Known: true, UsedPct: 100, ResetsAt: nrNow.Add(5 * time.Hour)}, false, nrNow)
	a.add(nextResetWindow{Known: true, UsedPct: 60, ResetsAt: nrNow.Add(100 * time.Hour)}, false, nrNow)
	w := a.window()
	if w == nil || math.Abs(w.UsedPercentage-60) > 1e-9 || w.ResetsAt != nrNow.Add(5*time.Hour).Unix() {
		t.Fatalf("window = %+v", w)
	}
}

func TestPooledAccumulatorRolledWindowCountsAsFree(t *testing.T) {
	var a pooledAccumulator
	a.add(nextResetWindow{Known: true, UsedPct: 100, ResetsAt: nrNow.Add(-time.Minute)}, false, nrNow)
	a.add(nextResetWindow{Known: true, UsedPct: 50, ResetsAt: nrNow.Add(time.Hour)}, false, nrNow)
	if w := a.window(); w.UsedPercentage != 25 {
		t.Fatalf("window = %+v", w)
	}
}

func TestPooledAccumulatorLatchedStaysFullAfterReset(t *testing.T) {
	var a pooledAccumulator
	a.add(nextResetWindow{Known: true, UsedPct: 100, ResetsAt: nrNow.Add(-time.Minute)}, true, nrNow)
	if w := a.window(); w.UsedPercentage != 100 {
		t.Fatalf("window = %+v", w)
	}
}

func TestPooledAccumulatorRejectedCountsFull(t *testing.T) {
	var a pooledAccumulator
	a.add(nextResetWindow{Known: true, UsedPct: 40, Rejected: true, ResetsAt: nrNow.Add(time.Hour)}, false, nrNow)
	if w := a.window(); w.UsedPercentage != 100 {
		t.Fatalf("window = %+v", w)
	}
}

func TestPooledAccumulatorEmptyIsNil(t *testing.T) {
	var a pooledAccumulator
	a.add(nextResetWindow{}, false, nrNow)
	if a.window() != nil {
		t.Fatal("unknown windows must not produce a pooled value")
	}
}

func TestPooledUsageForModelOnlyCountsCredentialsServingTheModel(t *testing.T) {
	withNextReset(t)
	reg := registry.GetGlobalRegistry()
	claudeModels := []*registry.ModelInfo{{ID: "claude-opus-5-5"}, {ID: "claude-fable-5-1"}}
	for _, id := range []string{"nr-c1", "nr-c2"} {
		reg.RegisterClient(id, "claude", claudeModels)
	}
	reg.RegisterClient("nr-x1", "codex", []*registry.ModelInfo{{ID: "gpt-5.5"}})
	t.Cleanup(func() {
		for _, id := range []string{"nr-c1", "nr-c2", "nr-x1"} {
			reg.UnregisterClient(id)
		}
	})
	m := NewManager(nil, nil, nil)
	for _, a := range []*Auth{
		claudeAuth("nr-c1", 20, 48*time.Hour),
		claudeAuth("nr-c2", 40, 24*time.Hour),
		codexAuth("nr-x1", 90, 10*time.Hour),
	} {
		if _, err := m.Register(context.Background(), a); err != nil {
			t.Fatal(err)
		}
	}
	nextResetPolled.set("nr-c1", nextResetSnapshot{Fable: nextResetWindow{Known: true, UsedPct: 80, ResetsAt: nrNow.Add(72 * time.Hour)}, ObservedAt: nrNow.Add(-time.Hour)})

	got := m.PooledUsageForModel("claude-opus-5-5", nrNow)
	if got.Accounts != 2 || got.SevenDay == nil || math.Abs(got.SevenDay.UsedPercentage-30) > 1e-9 ||
		got.SevenDay.ResetsAt != nrNow.Add(24*time.Hour).Unix() {
		t.Fatalf("opus pool = %+v %+v", got, got.SevenDay)
	}
	// For Fable, c1 counts at its Fable sub-limit (80) instead of 20.
	if got = m.PooledUsageForModel("claude-fable-5-1", nrNow); math.Abs(got.SevenDay.UsedPercentage-60) > 1e-9 {
		t.Fatalf("fable pool = %+v", got.SevenDay)
	}
	if got = m.PooledUsageForModel("gpt-5.5", nrNow); got.Accounts != 1 || math.Abs(got.SevenDay.UsedPercentage-90) > 1e-9 {
		t.Fatalf("codex pool = %+v %+v", got, got.SevenDay)
	}
}
