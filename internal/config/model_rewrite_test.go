package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestModelRewriteGlobMatch(t *testing.T) {
	for _, tc := range []struct {
		pattern, value string
		want           bool
	}{
		{"*sonnet*", "claude-sonnet-4-5", true},
		{"*sonnet*", "CLAUDE-SONNET-4", true},
		{"*SONNET*", "claude-sonnet-4", true},
		{"*sonnet*", "anthropic/claude-sonnet-4", true},
		{"*sonnet*", "sonnet", true},
		{"*sonnet*", "claude-opus-4-7", false},
		{"claude-*", "claude-opus-4-7", true},
		{"claude-*", "anthropic/claude-opus", false},
		{"*-4-7", "claude-opus-4-7", true},
		{"claude-*-4-*", "claude-opus-4-7", true},
		{"claude-*-4-*", "claude-opus-5-5", false},
		{"a*a", "a", false},
		{"a*a", "aa", true},
		{"exact-model", "EXACT-MODEL", true},
		{"exact-model", "exact-model-2", false},
		{"*", "anything/at-all", true},
	} {
		if got := modelRewriteGlobMatch(tc.pattern, tc.value); got != tc.want {
			t.Errorf("modelRewriteGlobMatch(%q, %q) = %t, want %t", tc.pattern, tc.value, got, tc.want)
		}
	}
}

func TestMatchModelRewriteFirstMatchWinsAndIsSinglePass(t *testing.T) {
	rules := []ModelRewriteRule{
		{Match: "*sonnet*", To: "claude-sonnet-5-5"},
		{Match: "*opus*", To: "claude-opus-5-5"},
		{Match: "*claude*", To: "should-not-win"},
	}
	for model, want := range map[string]string{
		"claude-sonnet-4-5":         "claude-sonnet-5-5",
		"anthropic/claude-sonnet-4": "claude-sonnet-5-5",
		"claude-opus-4-7":           "claude-opus-5-5",
		"claude-haiku-4-5":          "should-not-win",
	} {
		got, ok := MatchModelRewrite(rules, model)
		if !ok || got != want {
			t.Errorf("MatchModelRewrite(%q) = %q, %t; want %q", model, got, ok, want)
		}
	}
	if got, ok := MatchModelRewrite(rules, "gpt-5.5"); ok {
		t.Errorf("MatchModelRewrite(gpt-5.5) = %q, want no match", got)
	}

	// A -> B and B -> C must give B for A: the target is not matched again.
	chain := []ModelRewriteRule{{Match: "model-a", To: "model-b"}, {Match: "model-b", To: "model-c"}}
	if got, _ := MatchModelRewrite(chain, "model-a"); got != "model-b" {
		t.Fatalf("chained rewrite = %q, want model-b", got)
	}
}

func TestSanitizeModelRewrite(t *testing.T) {
	cfg := &Config{}
	cfg.Routing.ModelRewrite = []ModelRewriteRule{
		{Match: "  *sonnet*  ", To: " claude-sonnet-5-5 "},
		{Match: "", To: "claude-opus-5-5"},
		{Match: "*opus*", To: "   "},
		{Match: "*opus*", To: "claude-opus-5-5"},
	}
	cfg.SanitizeModelRewrite()
	want := []ModelRewriteRule{{Match: "*sonnet*", To: "claude-sonnet-5-5"}, {Match: "*opus*", To: "claude-opus-5-5"}}
	if !reflect.DeepEqual(cfg.Routing.ModelRewrite, want) {
		t.Fatalf("sanitized = %#v, want %#v", cfg.Routing.ModelRewrite, want)
	}
	cfg.Routing.ModelRewrite = []ModelRewriteRule{{Match: " ", To: "x"}}
	cfg.SanitizeModelRewrite()
	if cfg.Routing.ModelRewrite != nil {
		t.Fatalf("all-invalid rules = %#v, want nil", cfg.Routing.ModelRewrite)
	}
}

const modelRewriteV8YAML = `config-version: 8
routing:
  strategy: fill-first
  # rewrite every sonnet/opus id
  model-rewrite:
    - match: " *sonnet* "
      to: claude-sonnet-5-5
    - match: ""
      to: dropped
    - match: "*opus*"
      to: claude-opus-5-5
`

func TestModelRewriteParseAndLoadSanitize(t *testing.T) {
	want := []ModelRewriteRule{{Match: "*sonnet*", To: "claude-sonnet-5-5"}, {Match: "*opus*", To: "claude-opus-5-5"}}
	if errValidate := ValidateV8Config([]byte(modelRewriteV8YAML)); errValidate != nil {
		t.Fatalf("ValidateV8Config: %v", errValidate)
	}
	parsed, errParse := ParseConfigBytes([]byte(modelRewriteV8YAML))
	if errParse != nil {
		t.Fatalf("ParseConfigBytes: %v", errParse)
	}
	if !reflect.DeepEqual(parsed.Routing.ModelRewrite, want) {
		t.Fatalf("parsed rules = %#v, want %#v", parsed.Routing.ModelRewrite, want)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if errWrite := os.WriteFile(path, []byte(modelRewriteV8YAML), 0600); errWrite != nil {
		t.Fatalf("write: %v", errWrite)
	}
	loaded, errLoad := LoadConfig(path)
	if errLoad != nil {
		t.Fatalf("LoadConfig: %v", errLoad)
	}
	if !reflect.DeepEqual(loaded.Routing.ModelRewrite, want) {
		t.Fatalf("loaded rules = %#v, want %#v", loaded.Routing.ModelRewrite, want)
	}
}

func TestModelRewriteYAMLRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if errWrite := os.WriteFile(path, []byte(modelRewriteV8YAML), 0600); errWrite != nil {
		t.Fatalf("write: %v", errWrite)
	}
	cfg, errLoad := LoadConfig(path)
	if errLoad != nil {
		t.Fatalf("LoadConfig: %v", errLoad)
	}
	updated := []ModelRewriteRule{{Match: "*haiku*", To: "claude-haiku-5-5"}, {Match: "*sonnet*", To: "claude-sonnet-5-5"}}
	for _, rules := range [][]ModelRewriteRule{cfg.Routing.ModelRewrite, updated, nil} {
		cfg.Routing.ModelRewrite = rules
		if errSave := SaveConfigPreserveComments(path, cfg, true); errSave != nil {
			t.Fatalf("save: %v", errSave)
		}
		data, errRead := os.ReadFile(path)
		if errRead != nil {
			t.Fatalf("read: %v", errRead)
		}
		if errValidate := ValidateV8Config(data); errValidate != nil {
			t.Fatalf("validate saved config: %v\n%s", errValidate, data)
		}
		reloaded, errReload := LoadConfig(path)
		if errReload != nil {
			t.Fatalf("reload: %v", errReload)
		}
		if !reflect.DeepEqual(reloaded.Routing.ModelRewrite, rules) {
			t.Fatalf("round-tripped rules = %#v, want %#v\n%s", reloaded.Routing.ModelRewrite, rules, data)
		}
		if reloaded.Routing.Strategy != "fill-first" {
			t.Fatalf("routing.strategy = %q after save, want fill-first", reloaded.Routing.Strategy)
		}
		if len(rules) > 0 && !strings.Contains(string(data), "model-rewrite:") {
			t.Fatalf("saved config lacks routing.model-rewrite:\n%s", data)
		}
	}
}

func TestModelRewriteLegacyLayoutRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := "port: 8317\nrequest-retry: 2\nrouting:\n  model-rewrite:\n    - match: '*opus*'\n      to: claude-opus-5-5\n"
	if errWrite := os.WriteFile(path, []byte(raw), 0600); errWrite != nil {
		t.Fatalf("write: %v", errWrite)
	}
	cfg, errLoad := LoadConfig(path)
	if errLoad != nil {
		t.Fatalf("LoadConfig: %v", errLoad)
	}
	want := []ModelRewriteRule{{Match: "*opus*", To: "claude-opus-5-5"}, {Match: "*sonnet*", To: "claude-sonnet-5-5"}}
	cfg.Routing.ModelRewrite = want
	if errSave := SaveConfigPreserveComments(path, cfg); errSave != nil {
		t.Fatalf("save: %v", errSave)
	}
	data, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatalf("read: %v", errRead)
	}
	if IsV8ConfigLayout(mustYAMLRoot(t, data)) {
		t.Fatalf("legacy save migrated the layout:\n%s", data)
	}
	reloaded, errReload := LoadConfig(path)
	if errReload != nil {
		t.Fatalf("reload: %v", errReload)
	}
	if !reflect.DeepEqual(reloaded.Routing.ModelRewrite, want) || reloaded.RequestRetry != 2 {
		t.Fatalf("legacy round trip = %#v retry %d, want %#v retry 2\n%s", reloaded.Routing.ModelRewrite, reloaded.RequestRetry, want, data)
	}
}

func mustYAMLRoot(t *testing.T, data []byte) *yaml.Node {
	t.Helper()
	var doc yaml.Node
	if errUnmarshal := yaml.Unmarshal(data, &doc); errUnmarshal != nil || len(doc.Content) == 0 {
		t.Fatalf("unmarshal: %v", errUnmarshal)
	}
	return doc.Content[0]
}

func TestSanitizeModelRewriteNode(t *testing.T) {
	var doc yaml.Node
	if errUnmarshal := yaml.Unmarshal([]byte(modelRewriteV8YAML), &doc); errUnmarshal != nil {
		t.Fatalf("unmarshal: %v", errUnmarshal)
	}
	if errSanitize := SanitizeModelRewriteNode(doc.Content[0]); errSanitize != nil {
		t.Fatalf("SanitizeModelRewriteNode: %v", errSanitize)
	}
	var out struct {
		Routing struct {
			ModelRewrite []ModelRewriteRule `yaml:"model-rewrite"`
		} `yaml:"routing"`
	}
	if errDecode := doc.Decode(&out); errDecode != nil {
		t.Fatalf("decode: %v", errDecode)
	}
	want := []ModelRewriteRule{{Match: "*sonnet*", To: "claude-sonnet-5-5"}, {Match: "*opus*", To: "claude-opus-5-5"}}
	if !reflect.DeepEqual(out.Routing.ModelRewrite, want) {
		t.Fatalf("sanitized node = %#v, want %#v", out.Routing.ModelRewrite, want)
	}
}
