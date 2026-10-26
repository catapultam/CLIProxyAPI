package config

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestParseConfigBytesClaudeGatewayModels(t *testing.T) {
	rows := "- id: gpt-6.1-sol\n  label: GPT 6.1 Sol\n  behaves-as: claude-fable-5-1\n  auto-compact-window: 500000\n  effort-level: xhigh\n  hide-from-discovery: false\n- id: other\n  behaves-as: claude-opus-5\n"
	indent := func(s, pad string) string {
		return pad + strings.ReplaceAll(strings.TrimSuffix(s, "\n"), "\n", "\n"+pad) + "\n"
	}
	for name, doc := range map[string]string{
		"legacy": "claude-code:\n  gateway-models:\n" + indent(rows, "    "),
		"v8":     "upstream:\n  claude:\n    gateway-models:\n" + indent(rows, "      "),
	} {
		t.Run(name, func(t *testing.T) {
			cfg, err := ParseConfigBytes([]byte(doc))
			if err != nil {
				t.Fatalf("ParseConfigBytes() error = %v", err)
			}
			got := cfg.ClaudeCode.GatewayModels
			if len(got) != 2 || got[0].ID != "gpt-6.1-sol" || got[0].Label != "GPT 6.1 Sol" || got[0].BehavesAs != "claude-fable-5-1" ||
				got[0].AutoCompactWindow != 500000 || got[0].EffortLevel != "xhigh" || got[0].Hidden() || !got[1].Hidden() {
				t.Fatalf("GatewayModels = %+v", got)
			}
			if name == "v8" {
				if err = ValidateV8Config([]byte("config-version: 8\n" + doc)); err != nil {
					t.Fatalf("ValidateV8Config() error = %v", err)
				}
			}
		})
	}
}

func TestClaudeGatewayModelsValidation(t *testing.T) {
	ok := func(id string) ClaudeGatewayModel { return ClaudeGatewayModel{ID: id, BehavesAs: "claude-opus-5"} }
	long := strings.Repeat("a", MaxClaudeGatewayFieldLength+1)
	tooMany := make([]ClaudeGatewayModel, MaxClaudeGatewayModels+1)
	for i := range tooMany {
		tooMany[i] = ok(fmt.Sprintf("model-%d", i))
	}
	bad := map[string][]ClaudeGatewayModel{
		"empty id":        {ok("")},
		"space":           {ok("gpt 1")},
		"control":         {ok("gpt\n1")},
		"wildcard":        {ok("gpt-*")},
		"parens":          {ok("gpt-1(high)")},
		"claude prefix":   {ok("claude-opus-5")},
		"Claude prefix":   {ok("Claude-x")},
		"long id":         {ok(long)},
		"at sign":         {ok("gpt@1")},
		"leading dash":    {ok("-gpt")},
		"comma":           {ok("gpt,1")},
		"unicode":         {ok("gpt-é")},
		"duplicate":       {ok("Gpt-1"), ok("gpt-1")},
		"long label":      {{ID: "a", Label: long, BehavesAs: "claude-opus-5"}},
		"control label":   {{ID: "a", Label: "x\ty", BehavesAs: "claude-opus-5"}},
		"no behaves":      {{ID: "a"}},
		"non-claude":      {{ID: "a", BehavesAs: "gpt-2"}},
		"bare claude":     {{ID: "a", BehavesAs: "claude-"}},
		"behaves suffix":  {{ID: "a", BehavesAs: "claude-opus-5(high)"}},
		"behaves space":   {{ID: "a", BehavesAs: "claude-opus 5"}},
		"behaves dot":     {{ID: "a", BehavesAs: "claude-opus-4.5"}},
		"behaves under":   {{ID: "a", BehavesAs: "claude-opus_5"}},
		"behaves code":    {{ID: "a", BehavesAs: "claude-x;rm"}},
		"window low":      {{ID: "a", BehavesAs: "claude-opus-5", AutoCompactWindow: 99999}},
		"window high":     {{ID: "a", BehavesAs: "claude-opus-5", AutoCompactWindow: 1000001}},
		"window negative": {{ID: "a", BehavesAs: "claude-opus-5", AutoCompactWindow: -1}},
		"effort":          {{ID: "a", BehavesAs: "claude-opus-5", EffortLevel: "max"}},
		"too many":        tooMany,
	}
	for name, rows := range bad {
		if err := (ClaudeCodeConfig{GatewayModels: rows}).ValidateGatewayModels(); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	good := []ClaudeGatewayModel{
		{ID: "gpt-6.1-sol", BehavesAs: "claude-fable-5-1", AutoCompactWindow: 100000, EffortLevel: "low"},
		{ID: "vendor/model:tag", Label: "Model", BehavesAs: "claude-opus-5", AutoCompactWindow: 1000000},
		{ID: "x", BehavesAs: "claude-opus-5", AutoCompactWindow: 0},
	}
	if err := (ClaudeCodeConfig{GatewayModels: good}).ValidateGatewayModels(); err != nil {
		t.Fatalf("valid rows rejected: %v", err)
	}
}

func TestParseConfigBytesRejectsInvalidGatewayModels(t *testing.T) {
	for name, doc := range map[string]string{
		"semantic":   "claude-code:\n  gateway-models:\n    - id: claude-x\n      behaves-as: claude-opus-5\n",
		"bad int":    "claude-code:\n  gateway-models:\n    - id: a\n      behaves-as: claude-opus-5\n      auto-compact-window: big\n",
		"bad bool":   "claude-code:\n  gateway-models:\n    - id: a\n      behaves-as: claude-opus-5\n      hide-from-discovery: maybe\n",
		"not a list": "claude-code:\n  gateway-models: nope\n",
	} {
		if _, err := ParseConfigBytes([]byte(doc)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	if err := ValidateV8Config([]byte("config-version: 8\nupstream:\n  claude:\n    gateway-models:\n      - id: claude-x\n        behaves-as: claude-opus-5\n")); err == nil {
		t.Error("ValidateV8Config accepted a claude- ID")
	}
}

func TestEffectiveHiddenModels(t *testing.T) {
	f := false
	tr := true
	cfg := ClaudeCodeConfig{
		HiddenModels: []string{"manual-*"},
		GatewayModels: []ClaudeGatewayModel{
			{ID: "default-hidden", BehavesAs: "claude-opus-5"},
			{ID: "explicit-hidden", BehavesAs: "claude-opus-5", HideFromDiscovery: &tr},
			{ID: "shown", BehavesAs: "claude-opus-5", HideFromDiscovery: &f},
		},
	}
	want := []string{"manual-*", "default-hidden", "explicit-hidden"}
	if got := cfg.EffectiveHiddenModels(); !reflect.DeepEqual(got, want) {
		t.Fatalf("EffectiveHiddenModels() = %v, want %v", got, want)
	}
	if len(cfg.HiddenModels) != 1 {
		t.Fatalf("raw HiddenModels mutated: %v", cfg.HiddenModels)
	}
	if got := (ClaudeCodeConfig{HiddenModels: []string{"a"}}).EffectiveHiddenModels(); !reflect.DeepEqual(got, []string{"a"}) {
		t.Fatalf("no gateway rows: %v", got)
	}
}
