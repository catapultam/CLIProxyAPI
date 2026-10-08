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
	a.add(nextResetWindow{Known: true, UsedPct: 20, ResetsAt: nrNow.Add(48 * time.Hour)}, false, nrNow, 1)
	a.add(nextResetWindow{Known: true, UsedPct: 100, ResetsAt: nrNow.Add(5 * time.Hour)}, false, nrNow, 1)
	a.add(nextResetWindow{Known: true, UsedPct: 60, ResetsAt: nrNow.Add(100 * time.Hour)}, false, nrNow, 1)
	w := a.window(false)
	if w == nil || math.Abs(w.UsedPercentage-60) > 1e-9 || w.ResetsAt != nrNow.Add(5*time.Hour).Unix() {
		t.Fatalf("window = %+v", w)
	}
}

func TestPooledAccumulatorRolledWindowCountsAsFree(t *testing.T) {
	var a pooledAccumulator
	a.add(nextResetWindow{Known: true, UsedPct: 100, ResetsAt: nrNow.Add(-time.Minute)}, false, nrNow, 1)
	a.add(nextResetWindow{Known: true, UsedPct: 50, ResetsAt: nrNow.Add(time.Hour)}, false, nrNow, 1)
	if w := a.window(false); w.UsedPercentage != 25 {
		t.Fatalf("window = %+v", w)
	}
}

func TestPooledAccumulatorLatchedStaysFullAfterReset(t *testing.T) {
	var a pooledAccumulator
	a.add(nextResetWindow{Known: true, UsedPct: 100, ResetsAt: nrNow.Add(-time.Minute)}, true, nrNow, 1)
	if w := a.window(false); w.UsedPercentage != 100 {
		t.Fatalf("window = %+v", w)
	}
}

func TestPooledAccumulatorRejectedCountsFull(t *testing.T) {
	var a pooledAccumulator
	a.add(nextResetWindow{Known: true, UsedPct: 40, Rejected: true, ResetsAt: nrNow.Add(time.Hour)}, false, nrNow, 1)
	if w := a.window(false); w.UsedPercentage != 100 {
		t.Fatalf("window = %+v", w)
	}
}

func TestPooledAccumulatorEmptyIsNil(t *testing.T) {
	var a pooledAccumulator
	a.add(nextResetWindow{}, false, nrNow, 1)
	if a.window(false) != nil {
		t.Fatal("unknown windows must not produce a pooled value")
	}
}

func TestPooledAccumulatorWeightedMeanWhenRequested(t *testing.T) {
	var a pooledAccumulator
	// size 10 at 80% used, size 90 at 10% used: plain mean 45%, weighted mean
	// (10*80 + 90*10)/100 = 17%.
	a.add(nextResetWindow{Known: true, UsedPct: 80, ResetsAt: nrNow.Add(time.Hour)}, false, nrNow, 10)
	a.add(nextResetWindow{Known: true, UsedPct: 10, ResetsAt: nrNow.Add(2 * time.Hour)}, false, nrNow, 90)
	if w := a.window(false); math.Abs(w.UsedPercentage-45) > 1e-9 {
		t.Fatalf("unweighted window = %+v, want 45", w)
	}
	if w := a.window(true); math.Abs(w.UsedPercentage-17) > 1e-9 {
		t.Fatalf("weighted window = %+v, want 17", w)
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
	if got = m.PooledUsageForProvider("claude", nrNow); got.Accounts != 2 || got.Provider != "claude" || math.Abs(got.SevenDay.UsedPercentage-30) > 1e-9 {
		t.Fatalf("claude provider pool = %+v %+v", got, got.SevenDay)
	}
	if got = m.PooledUsageForProvider("codex", nrNow); got.Accounts != 1 || math.Abs(got.SevenDay.UsedPercentage-90) > 1e-9 {
		t.Fatalf("codex provider pool = %+v %+v", got, got.SevenDay)
	}
}

func TestPooledUsageReportListsEveryProviderWeekly(t *testing.T) {
	withNextReset(t)
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient("rp-c1", "claude", []*registry.ModelInfo{{ID: "claude-opus-5-5"}})
	reg.RegisterClient("rp-c2", "claude", []*registry.ModelInfo{{ID: "claude-sonnet-5-5"}})
	reg.RegisterClient("rp-x1", "codex", []*registry.ModelInfo{{ID: "gpt-5.5"}})
	t.Cleanup(func() {
		for _, id := range []string{"rp-c1", "rp-c2", "rp-x1"} {
			reg.UnregisterClient(id)
		}
	})
	m := NewManager(nil, nil, nil)
	for _, a := range []*Auth{claudeAuth("rp-c1", 20, 48*time.Hour), claudeAuth("rp-c2", 60, 48*time.Hour), codexAuth("rp-x1", 50, 10*time.Hour)} {
		if _, err := m.Register(context.Background(), a); err != nil {
			t.Fatal(err)
		}
	}
	got := m.PooledUsageReport("claude-opus-5-5", nrNow)
	if got.Provider != "claude" || got.Accounts != 1 || len(got.SevenDayByProvider) != 2 {
		t.Fatalf("report = %+v", got)
	}
	// claude's weekly entry uses the model pool (only rp-c1 serves opus).
	if c := got.SevenDayByProvider[0]; c.Provider != "claude" || c.Accounts != 1 || math.Abs(c.SevenDay.UsedPercentage-20) > 1e-9 {
		t.Fatalf("claude entry = %+v %+v", c, c.SevenDay)
	}
	if o := got.SevenDayByProvider[1]; o.Provider != "openai" || o.Accounts != 1 || math.Abs(o.SevenDay.UsedPercentage-50) > 1e-9 {
		t.Fatalf("openai entry = %+v %+v", o, o.SevenDay)
	}
}

func TestPooledUsageForProviderWeightsBySizeWhenAllKnown(t *testing.T) {
	withNextReset(t)
	m := NewManager(nil, nil, nil)
	small := claudeAuth("sz-small", 80, 48*time.Hour)
	small.Attributes = map[string]string{AttributeSize: "10"}
	big := claudeAuth("sz-big", 10, 24*time.Hour)
	big.Attributes = map[string]string{AttributeSize: "90"}
	for _, a := range []*Auth{small, big} {
		if _, err := m.Register(context.Background(), a); err != nil {
			t.Fatal(err)
		}
	}
	// Plain mean would be 45%; size-weighted mean is (10*80 + 90*10)/100 = 17%.
	got := m.PooledUsageForProvider("claude", nrNow)
	if got.Accounts != 2 || got.SevenDay == nil || math.Abs(got.SevenDay.UsedPercentage-17) > 1e-9 {
		t.Fatalf("weighted pool = %+v %+v", got, got.SevenDay)
	}
}

func TestPooledUsageForProviderUnweightedWhenOneSizeUnknown(t *testing.T) {
	withNextReset(t)
	m := NewManager(nil, nil, nil)
	small := claudeAuth("sz-small2", 80, 48*time.Hour)
	small.Attributes = map[string]string{AttributeSize: "10"}
	// big has no size at all: the pool must fall back to the plain mean.
	big := claudeAuth("sz-big2", 10, 24*time.Hour)
	for _, a := range []*Auth{small, big} {
		if _, err := m.Register(context.Background(), a); err != nil {
			t.Fatal(err)
		}
	}
	got := m.PooledUsageForProvider("claude", nrNow)
	if got.Accounts != 2 || got.SevenDay == nil || math.Abs(got.SevenDay.UsedPercentage-45) > 1e-9 {
		t.Fatalf("unweighted pool = %+v %+v", got, got.SevenDay)
	}
}
